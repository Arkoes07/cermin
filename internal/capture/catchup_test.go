package capture_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
)

func TestBot_CatchUp_ClassifiesOnceClassifierRecovers(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{
		candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
		err:        errors.New("classifier down"),
	}
	var gotTimeout time.Duration
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		if deadline, ok := ctx.Deadline(); ok {
			gotTimeout = time.Until(deadline)
		}
		return classifier.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	classifier.err = nil
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	if gotTimeout <= 8*time.Second || gotTimeout > 60*time.Second {
		t.Errorf("classified with %v left, want more than Capture's 8s and at most 60s", gotTimeout)
	}
	classifications, err := db.Classifications(ctx, thought.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("Classifications = %+v, %v; want 1", classifications, err)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "✓ saved", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: 1001, Text: "💡 idea · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_CatchUp_RetriesLateClassificationEdit(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{
		candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
		err:        errors.New("classifier down"),
	}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	classifier.err = nil
	api.failNextEdit()
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if edits := api.editedMessages(); len(edits) != 0 {
		t.Fatalf("edited %+v, want the edit failed", edits)
	}
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp again: %v", err)
	}

	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "💡 idea · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
	if classifications, err := db.Classifications(ctx, thought.ID); err != nil || len(classifications) != 1 {
		t.Errorf("Classifications = %+v, %v; want 1", classifications, err)
	}
	if calls := len(classifier.gotTopics); calls != 2 {
		t.Errorf("classified %d times, want twice: at Capture and by the first CatchUp", calls)
	}
}

func TestBot_CatchUp_LeavesDeletedAcknowledgementDeleted(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	api.deleteMessage(ackID)
	api.queue(commandUpdate(t, commandID, "/kind plan", messageID))
	processQueued(t, bot)
	for range 2 {
		if err := bot.CatchUp(ctx); err != nil {
			t.Fatalf("CatchUp: %v", err)
		}
	}

	wantRefused := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · running"}}
	if refused := api.refusedEdits(); !slices.Equal(refused, wantRefused) {
		t.Errorf("tried edits %+v, want only %+v", refused, wantRefused)
	}
	if sent := api.sentMessages(); len(sent) != 1 {
		t.Errorf("sent %+v, want only the Acknowledgement, and none to replace it", sent)
	}
	classifications, err := db.Classifications(ctx, thought.ID)
	if err != nil || len(classifications) != 2 || classifications[1].Kind != "plan" {
		t.Errorf("Classifications = %+v, %v; want Cermin's and the Correction to plan", classifications, err)
	}
}

func TestBot_CatchUp_LeavesDeletedThoughtsAcknowledgement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	captureQueued(t, bot, db)
	// The Correction's edit fails, so the Acknowledgement is out of date when
	// you delete the Thought.
	api.failNextEdit()
	api.queue(commandUpdate(t, commandID, "/kind plan", messageID))
	api.queue(commandUpdate(t, commandID+1, "/delete", messageID))
	processQueued(t, bot)

	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "🗑 deleted"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_CatchUp_SendsMissingAcknowledgement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		classifierErr error
		wantAck       string
	}{
		{
			name:    "classified before the crash",
			wantAck: "💡 idea · running",
		},
		{
			name:          "unclassified",
			classifierErr: errors.New("classifier down"),
			wantAck:       "✓ saved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{
				candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
				err:        tt.classifierErr,
			}
			bot, db, api := newBot(t, classifier.classify)
			// A failed reply leaves what a crash before replying does: the
			// Thought saved without an Acknowledgement.
			api.failNextSend()
			api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
			in := pollUpdate(t, bot, db)
			if err := bot.Process(ctx, in); err != nil {
				t.Fatalf("Process: %v", err)
			}

			for range 2 {
				if err := bot.CatchUp(ctx); err != nil {
					t.Fatalf("CatchUp: %v", err)
				}
			}
			wantSent := []sentMessage{{ChatID: ownerID, Text: tt.wantAck, ReplyToID: messageID}}
			if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
				t.Errorf("sent %+v, want %+v", sent, wantSent)
			}

			// After a crash before the update was marked processed, a restart
			// processes it again, and must not send another.
			if err := bot.Process(ctx, in); err != nil {
				t.Fatalf("Process again: %v", err)
			}
			if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
				t.Errorf("after processing again, sent %+v, want %+v", sent, wantSent)
			}
		})
	}
}

func TestBot_CatchUp_RetriesFailedClassification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	running := []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}
	classifier := &fakeClassifier{candidates: running, err: errors.New("classifier down")}
	bot, db, api := newBot(t, classifier.classify)
	now := thoughtTime
	bot.SetTimeNow(func() time.Time { return now })
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	failures := []struct {
		name       string
		err        error
		kind       string
		candidates []cermin.TopicCandidate
	}{
		{name: "classifier down", err: errors.New("classifier down"), candidates: running},
		{name: "classifier timing out", err: context.DeadlineExceeded, candidates: running},
		{name: "no topic", candidates: nil},
		{name: "unknown kind", kind: "rant", candidates: running}, // fails as it is saved
	}
	// Each failure follows the ones before it on the same Thought, so these
	// subtests run in order.
	for _, f := range failures {
		t.Run(f.name, func(t *testing.T) {
			ctx := t.Context()
			classifier.err, classifier.kind, classifier.candidates = f.err, f.kind, f.candidates
			// A day apart, so the Thought is due at each catch-up however
			// long the one before postponed it.
			catchUpEvery(t, bot, &now, 24*time.Hour, 1)
			if classifications, err := db.Classifications(ctx, thought.ID); err != nil || len(classifications) != 0 {
				t.Errorf("Classifications = %+v, %v; want none", classifications, err)
			}
			if topics, err := db.TopicNames(ctx); err != nil || len(topics) != 0 {
				t.Errorf("TopicNames = %q, %v; want none", topics, err)
			}
			if edits := api.editedMessages(); len(edits) != 0 {
				t.Errorf("edited %+v, want nothing", edits)
			}
		})
	}

	classifier.err, classifier.kind, classifier.candidates = nil, "", running
	catchUpEvery(t, bot, &now, 24*time.Hour, 2)
	if classifications, err := db.Classifications(ctx, thought.ID); err != nil || len(classifications) != 1 {
		t.Errorf("Classifications = %+v, %v; want 1", classifications, err)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: 1001, Text: "💡 idea · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_CatchUp_ClassifiesAtMost20(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{
		candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
		err:        errors.New("classifier down"),
	}
	bot, db, api := newBot(t, classifier.classify)
	for i := range 21 {
		api.queue(update(t, "message", ownerID, map[string]any{
			"message_id": messageID + i,
			"text":       "Run " + strconv.Itoa(i+1) + " km on Saturday.",
		}))
	}
	captureQueued(t, bot, db)

	classifier.err, classifier.gotTopics = nil, nil
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	if calls := len(classifier.gotTopics); calls != 20 {
		t.Errorf("first CatchUp classified %d Thoughts, want 20", calls)
	}
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp again: %v", err)
	}
	if calls := len(classifier.gotTopics); calls != 21 {
		t.Errorf("classified %d Thoughts after two CatchUps, want all 21", calls)
	}
}

func TestBot_CatchUp_BacksOffFromThoughtClassifierAlwaysFailsOn(t *testing.T) {
	t.Parallel()
	classifier := &fakeClassifier{err: errors.New("classifier refused")}
	bot, db, api := newBot(t, classifier.classify)
	now := thoughtTime
	bot.SetTimeNow(func() time.Time { return now })
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	captureQueued(t, bot, db)

	classifier.gotTopics = nil // Capture's call doesn't count
	catchUpEvery(t, bot, &now, time.Minute, 24*60)
	if calls := len(classifier.gotTopics); calls != 11 {
		t.Errorf("classified %d times in the first 24 hours, want 11, waiting 1, 2, 4 … 512 minutes between them", calls)
	}
	catchUpEvery(t, bot, &now, time.Minute, 24*60)
	if calls := len(classifier.gotTopics); calls != 12 {
		t.Errorf("classified %d times in 48 hours, want 12, once in the second 24 hours", calls)
	}
	// Hourly from here, to cover days cheaply: the wait stops doubling at 24
	// hours, where it would otherwise reach 34 hours, then 68.
	catchUpEvery(t, bot, &now, time.Hour, 3*24)
	if calls := len(classifier.gotTopics); calls != 15 {
		t.Errorf("classified %d times in 5 days, want 15, once a day after the first 2", calls)
	}
}

func TestBot_CatchUp_ClassifiesWithinAnHourOfOutageEnding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{
		candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
		err:        errors.New("classifier down"),
	}
	bot, db, api := newBot(t, classifier.classify)
	now := thoughtTime
	bot.SetTimeNow(func() time.Time { return now })

	// The classifier is down for the first hour. Thoughts come in at its
	// start, halfway through and in its last minute.
	for minute := range 2*60 + 1 {
		switch minute {
		case 0, 30, 59:
			api.queue(update(t, "message", ownerID, map[string]any{
				"message_id": messageID + minute,
				"text":       "Run " + strconv.Itoa(minute+1) + " km on Saturday.",
			}))
			processQueued(t, bot)
		case 60:
			classifier.err, classifier.gotTopics = nil, nil
		}
		catchUpEvery(t, bot, &now, time.Minute, 1)
	}

	thoughts, err := db.Thoughts(ctx)
	if err != nil || len(thoughts) != 3 {
		t.Fatalf("Thoughts = %+v, %v; want 3", thoughts, err)
	}
	for i, classifications := range classificationsOf(t, db, thoughts) {
		if len(classifications) != 1 {
			t.Errorf("Thought %d has %d Classifications an hour after the outage, want 1", thoughts[i].ID, len(classifications))
		}
	}
	if calls := len(classifier.gotTopics); calls != 3 {
		t.Errorf("classified %d times after the outage, want once per Thought", calls)
	}
}

func TestBot_CatchUp_ClassifiesNewThoughtBehind20Failing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const newText = "Buy new running shoes."
	classifier := &fakeClassifier{
		candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}},
		err:        errors.New("classifier down"),
	}
	// Once up, the classifier refuses every Thought but the new one.
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		if text != newText {
			return cermin.Classification{}, errors.New("classifier refused")
		}
		return classifier.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	now := thoughtTime
	bot.SetTimeNow(func() time.Time { return now })
	for i := range 20 {
		api.queue(update(t, "message", ownerID, map[string]any{
			"message_id": messageID + i,
			"text":       "Run " + strconv.Itoa(i+1) + " km on Saturday.",
		}))
	}
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id": messageID + 20,
		"text":       newText,
	}))
	newThought := captureQueued(t, bot, db)

	classifier.err = nil
	catchUpEvery(t, bot, &now, time.Minute, 3)
	if classifications, err := db.Classifications(ctx, newThought.ID); err != nil || len(classifications) != 1 {
		t.Errorf("Classifications of the new Thought after 3 catch-ups = %+v, %v; want 1", classifications, err)
	}
}

// The race tests below need no goroutines: the catch-up calls the classifier
// without the lock, so a fake classifier runs the competing step inside its
// own call, then answers.

func TestBot_CatchUp_KeepsClassificationMadeMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	late := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "errands", Confidence: 0.9}}}
	first := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	var bot *capture.Bot
	var in sqlite.InboxUpdate
	calls := 0
	classify := func(
		_ context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		calls++
		switch calls {
		case 1: // at Capture
			return cermin.Classification{}, errors.New("classifier down")
		case 2: // the catch-up's
			// Meanwhile a restart after a crash processes the update again,
			// which classifies the Thought itself.
			if err := bot.Process(ctx, in); err != nil {
				t.Fatalf("Process again: %v", err)
			}
			return late.classify(ctx, text, kinds, topics)
		default: // at Capture again
			return first.classify(ctx, text, kinds, topics)
		}
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	in = pollUpdate(t, bot, db)
	api.failNextSend()
	if err := bot.Process(ctx, in); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// The catch-up's Acknowledgement fails too, so the Thought still has none
	// when it is classified.
	api.failNextSend()
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	thoughts, err := db.Thoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("Thoughts = %+v, %v; want 1", thoughts, err)
	}
	classifications, err := db.Classifications(ctx, thoughts[0].ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("Classifications = %+v, %v; want 1", classifications, err)
	}
	wantTopics := []cermin.TopicCandidate{{Name: "running", Confidence: 0.9, Kept: true}}
	if got := classifications[0].Topics; !slices.Equal(got, wantTopics) {
		t.Errorf("kept the Classification with Topics %+v, want the Capture's %+v", got, wantTopics)
	}
	if topics, err := db.TopicNames(ctx); err != nil || !slices.Equal(topics, []string{"running"}) {
		t.Errorf("TopicNames = %q, %v; want running only", topics, err)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	if edits := api.editedMessages(); len(edits) != 0 {
		t.Errorf("edited %+v, want nothing", edits)
	}
}

func TestBot_CatchUp_SendsOneAcknowledgementWhenCaptureRaces(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	late := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	var bot *capture.Bot
	var in sqlite.InboxUpdate
	calls := 0
	classify := func(
		_ context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		calls++
		if calls != 2 { // at Capture, and at Capture again
			return cermin.Classification{}, errors.New("classifier down")
		}
		// The catch-up's. Meanwhile a restart after a crash processes the
		// update again, which sends the Acknowledgement.
		if err := bot.Process(ctx, in); err != nil {
			t.Fatalf("Process again: %v", err)
		}
		return late.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	in = pollUpdate(t, bot, db)
	api.failNextSend()
	if err := bot.Process(ctx, in); err != nil {
		t.Fatalf("Process: %v", err)
	}

	// The catch-up's Acknowledgement fails too, so the Thought still has none
	// when it is classified.
	api.failNextSend()
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	// The catch-up edits the Acknowledgement the Capture sent, rather than
	// sending a second.
	wantSent := []sentMessage{{ChatID: ownerID, Text: "✓ saved", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "💡 idea · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_CatchUp_DropsClassificationOfThoughtDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	late := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	var bot *capture.Bot
	var api *fakeBotAPI
	calls := 0
	classify := func(
		_ context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		calls++
		if calls != 2 { // at each Capture
			return cermin.Classification{}, errors.New("classifier down")
		}
		// The catch-up's. Meanwhile you delete the Thought and capture
		// another, which the classifier fails on too.
		api.queue(commandUpdate(t, commandID, "/delete", messageID))
		api.queue(update(t, "message", ownerID, map[string]any{
			"message_id": commandID + 1, // after the /delete
			"text":       "Buy new running shoes.",
		}))
		processQueued(t, bot)
		return late.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	deleted := captureQueued(t, bot, db)

	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	thoughts, err := db.Thoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("Thoughts = %+v, %v; want only the new one", thoughts, err)
	}
	// Were the deleted Thought's ID reused, the catch-up would find the new
	// Thought under it, unclassified, and give it the deleted one's
	// Classification.
	if thoughts[0].ID == deleted.ID {
		t.Errorf("new Thought has the deleted Thought's ID %d, want a new one", deleted.ID)
	}
	if got, err := db.Classifications(ctx, thoughts[0].ID); err != nil || len(got) != 0 {
		t.Errorf("Classifications of the new Thought = %+v, %v; want none, not the deleted Thought's", got, err)
	}
	if topics, err := db.TopicNames(ctx); err != nil || len(topics) != 0 {
		t.Errorf("TopicNames = %q, %v; want none", topics, err)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "🗑 deleted"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}
