package capture_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/cermin"
)

func TestBot_Run_ProcessesWhileNextPollWaits(t *testing.T) {
	t.Parallel()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	var heldPolls <-chan struct{}
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		select {
		case <-heldPolls:
		case <-time.After(5 * time.Second):
			t.Error("classifying with no poll waiting, want the next poll under way")
		}
		return classifier.classify(ctx, text, kinds, topics)
	}
	bot, _, api := newBot(t, classify)
	heldPolls = api.holdEmptyPolls()
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))

	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() {
		bot.Run(ctx)
		close(stopped)
	}()
	defer func() {
		cancel()
		<-stopped
	}()

	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Equal(api.sentMessages(), wantSent) {
		if time.Now().After(deadline) {
			t.Fatalf("sent %+v, want %+v", api.sentMessages(), wantSent)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBot_Poll_CutsCopiesOfOtherMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, repliedText, 0))
	captureQueued(t, bot, db)

	// A Correction quoting part of the Thought it replies to.
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id":       commandID,
		"text":             "/kind plan",
		"reply_to_message": repliedMessage(ownerID, messageID),
		"quote":            map[string]any{"text": "packed at 8am", "position": 18},
	}))
	in := pollUpdate(t, bot, db)

	var stored struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(in.Payload, &stored); err != nil {
		t.Fatalf("decoding stored update: %v", err)
	}
	if got, want := string(stored.Message["reply_to_message"]), `{"message_id":7}`; got != want {
		t.Errorf("stored reply_to_message = %s, want %s", got, want)
	}
	if quote, isStored := stored.Message["quote"]; isStored {
		t.Errorf("stored quote %s, want none", quote)
	}
	if err := bot.Process(ctx, in); err != nil {
		t.Fatalf("Process: %v", err)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_ServeHealth_FailsOncePollingStops(t *testing.T) {
	t.Parallel()
	bot, _, _ := newBot(t, nil) // polling never classifies
	now := thoughtTime
	bot.SetTimeNow(func() time.Time { return now })
	checkHealth := func() int {
		rec := httptest.NewRecorder()
		bot.ServeHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return rec.Code
	}

	if code := checkHealth(); code != http.StatusServiceUnavailable {
		t.Errorf("before the first poll: status %d, want 503", code)
	}
	if err := bot.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	now = now.Add(2*time.Minute - time.Second)
	if code := checkHealth(); code != http.StatusOK {
		t.Errorf("1m59s after a poll: status %d, want 200", code)
	}
	now = now.Add(time.Second)
	if code := checkHealth(); code != http.StatusServiceUnavailable {
		t.Errorf("2m after the last poll: status %d, want 503", code)
	}
	if err := bot.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if code := checkHealth(); code != http.StatusOK {
		t.Errorf("after a fresh poll: status %d, want 200", code)
	}
}

func TestBot_ProcessInbox_SkipsUpdateThatKeepsFailing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const bugText = "Run 5 km on Saturday."
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		if text == bugText {
			panic("a bug in the classifier")
		}
		return classifier.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, bugText, 0))
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id": messageID + 1,
		"text":       "Buy new running shoes.",
	}))
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	// The bug crashes Cermin, and the first update with it, at each restart.
	for attempt := 1; attempt <= 5; attempt++ {
		processInboxPanicking(t, bot)
		updates, err := db.GetUnprocessedUpdates(ctx)
		if err != nil || len(updates) != 2 {
			t.Fatalf("after attempt %d, %d unprocessed updates, %v; want both", attempt, len(updates), err)
		}
		if got := updates[0].Attempts; got != attempt {
			t.Fatalf("after attempt %d, the first update has %d attempts, want %d", attempt, got, attempt)
		}
	}
	if err := bot.ProcessInbox(ctx); err != nil {
		t.Fatalf("ProcessInbox after 5 attempts: %v", err)
	}

	wantSent := []sentMessage{
		{ChatID: ownerID, Text: "Couldn't process this message. It's kept for a replay.", ReplyToID: messageID},
		{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID + 1},
	}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
}

// processInboxPanicking calls ProcessInbox, which is to panic as a bug that
// crashes Cermin does, and recovers.
func processInboxPanicking(t *testing.T, bot *capture.Bot) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Fatal("ProcessInbox didn't panic")
		}
	}()
	if err := bot.ProcessInbox(t.Context()); err != nil {
		t.Errorf("ProcessInbox: %v", err)
	}
}

func TestBot_ProcessInbox_MarksReplyProcessedWhenSendFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		update string
	}{
		{name: "help", update: commandUpdate(t, commandID, "/help", 0)},
		{name: "delete not a reply", update: commandUpdate(t, commandID, "/delete", 0)},
		{
			name: "edit of a message that isn't a thought",
			update: withUpdateID(t, update(t, "edited_message", ownerID, map[string]any{
				"text":      "Start running on Sundays.",
				"edit_date": thoughtTime.Unix() + 60,
			}), commandID),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{}
			bot, db, api := newBot(t, classifier.classify)
			api.failNextSend()
			api.queue(tt.update)

			processQueued(t, bot)

			if sent := api.sentMessages(); len(sent) != 0 {
				t.Errorf("sent %+v, want the reply failed", sent)
			}
			if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_ProcessInbox_CapturesOwnerText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{
		{Name: "transport", Confidence: 0.95},
		{Name: "cities", Confidence: 0.8},
	}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "What if bus stops showed how full the next bus is?", 5))

	before := time.Now().Truncate(time.Millisecond)
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := bot.ProcessInbox(ctx); err != nil {
		t.Fatalf("ProcessInbox: %v", err)
	}
	after := time.Now()

	thoughts, err := db.GetThoughts(ctx)
	if err != nil {
		t.Fatalf("GetThoughts: %v", err)
	}
	if len(thoughts) != 1 {
		t.Fatalf("got %d Thoughts, want 1", len(thoughts))
	}
	got := thoughts[0]
	if !got.ThoughtAt.Equal(thoughtTime) {
		t.Errorf("ThoughtAt = %v, want %v", got.ThoughtAt, thoughtTime)
	}
	if got.ReceivedAt.Before(before) || got.ReceivedAt.After(after) {
		t.Errorf("ReceivedAt = %v, want between %v and %v", got.ReceivedAt, before, after)
	}
	got.ThoughtAt, got.ReceivedAt = time.Time{}, time.Time{}
	want := cermin.Thought{
		ID:                  got.ID,
		Text:                "What if bus stops showed how full the next bus is?",
		Source:              "telegram",
		TGMessageID:         messageID,
		TGReplyToMessageID:  5,
		AckMessageID:        1001,
		AckClassificationID: got.AckClassificationID, // shown by what was sent
	}
	if got != want {
		t.Errorf("Thought = %+v, want %+v", got, want)
	}

	if actions := api.chatActions(); !slices.Equal(actions, []string{"typing"}) {
		t.Errorf("chat actions = %q, want typing", actions)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · transport, cities", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}

	classifications, err := db.GetClassificationsByThoughtID(ctx, got.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want 1", classifications, err)
	}
	gotClassification := classifications[0]
	if gotClassification.CreatedAt.Before(before) || gotClassification.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", gotClassification.CreatedAt, before, after)
	}
	gotClassification.ID, gotClassification.CreatedAt = 0, time.Time{}
	wantClassification := cermin.Classification{
		ThoughtID:      got.ID,
		Source:         cermin.SourceCermin,
		Kind:           "idea",
		KindConfidence: 0.9,
		Topics: []cermin.TopicCandidate{
			{Name: "transport", Confidence: 0.95, Kept: true},
			{Name: "cities", Confidence: 0.8, Kept: true},
		},
		Model:            "claude-test",
		PromptVersion:    "classify-v1",
		ConfidenceMethod: "verbalized-v1",
		RawOutput:        fakeRawOutput,
		Language:         "en",
	}
	if !reflect.DeepEqual(gotClassification, wantClassification) {
		t.Errorf("Classification = %+v, want %+v", gotClassification, wantClassification)
	}
}

func TestBot_Process_CountsThoughtOnce(t *testing.T) {
	// Not parallel: the counter is global, and no parallel test runs while
	// this one does.
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	in := pollUpdate(t, bot, db)

	before := testutil.ToFloat64(capture.ThoughtsCaptured)
	for range 2 {
		if err := bot.Process(t.Context(), in); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}
	if got := testutil.ToFloat64(capture.ThoughtsCaptured) - before; got != 1 {
		t.Errorf("cermin_thoughts_captured_total rose by %v, want 1", got)
	}
}

func TestBot_ProcessInbox_CapturesForwardedTextAtForwardTime(t *testing.T) {
	t.Parallel()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(update(t, "message", ownerID, map[string]any{
		"text": "Parkrun starts at 7am every Saturday.",
		"forward_origin": map[string]any{
			"type":             "hidden_user",
			"sender_user_name": "A Friend",
			"date":             thoughtTime.Add(-24 * time.Hour).Unix(),
		},
	}))

	thought := captureQueued(t, bot, db)

	if thought.Text != "Parkrun starts at 7am every Saturday." {
		t.Errorf("Text = %q, want the forwarded text", thought.Text)
	}
	if !thought.ThoughtAt.Equal(thoughtTime) {
		t.Errorf("ThoughtAt = %v, want %v, when it was forwarded", thought.ThoughtAt, thoughtTime)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
}

// The keep rule itself is specified by the domain's tests. This one checks
// only that Capture stores and shows what the rule returns, so it holds
// however the rule is tuned.
func TestBot_ProcessInbox_KeepsStrongTopics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// Out of order, with more clearly strong candidates than a Thought keeps
	// and a clearly weak one.
	candidates := []cermin.TopicCandidate{
		{Name: "design", Confidence: 0.1},
		{Name: "cities", Confidence: 0.85},
		{Name: "transport", Confidence: 0.95},
		{Name: "data", Confidence: 0.8},
		{Name: "buses", Confidence: 0.9},
	}
	wantStored := cermin.KeepTopics(candidates, nil)
	kept := cermin.Classification{Topics: wantStored}.KeptTopics()
	classifier := &fakeClassifier{candidates: candidates}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "What if bus stops showed how full the next bus is?", 0))

	thought := captureQueued(t, bot, db)

	wantAck := "💡 idea · " + strings.Join(kept, ", ")
	wantSent := []sentMessage{{ChatID: ownerID, Text: wantAck, ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	classifications, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want 1", classifications, err)
	}
	if got := classifications[0].Topics; !slices.Equal(got, wantStored) {
		t.Errorf("stored candidates %+v, want %+v", got, wantStored)
	}
	wantTopics := slices.Sorted(slices.Values(kept))
	if got, err := db.GetTopicNames(ctx); err != nil || !slices.Equal(got, wantTopics) {
		t.Errorf("GetTopicNames = %q, %v; want %q", got, err, wantTopics)
	}
}

func TestBot_ProcessInbox_ReusesTopicIgnoringCase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "business", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "What if small shops could restock by chat?", 0))
	captureQueued(t, bot, db)

	classifier.candidates = []cermin.TopicCandidate{
		{Name: "Business", Confidence: 0.85},
		{Name: "retail", Confidence: 0.8},
	}
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id": messageID + 1,
		"text":       "Corner shops could share one delivery van.",
	}))
	thought := captureQueued(t, bot, db)

	if len(classifier.gotTopics) != 2 {
		t.Fatalf("classified %d times, want twice", len(classifier.gotTopics))
	}
	if got := classifier.gotTopics[1]; !slices.Equal(got, []string{"business"}) {
		t.Errorf("second Thought classified with existing Topics %q, want business", got)
	}
	if sent := api.sentMessages(); len(sent) != 2 || sent[1].Text != "💡 idea · business, retail" {
		t.Errorf("sent %+v, want a second Acknowledgement 💡 idea · business, retail", sent)
	}
	classifications, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want 1", classifications, err)
	}
	wantStored := []cermin.TopicCandidate{
		{Name: "business", Confidence: 0.85, Kept: true},
		{Name: "retail", Confidence: 0.8, Kept: true},
	}
	if got := classifications[0].Topics; !slices.Equal(got, wantStored) {
		t.Errorf("stored candidates %+v, want %+v", got, wantStored)
	}
	if got, err := db.GetTopicNames(ctx); err != nil || !slices.Equal(got, []string{"business", "retail"}) {
		t.Errorf("GetTopicNames = %q, %v; want business, retail", got, err)
	}
}

func TestBot_ProcessInbox_NeverKeepsArchivedTopic(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	path := filepath.Join(t.TempDir(), "cermin.db")
	bot, db, api := newBotAt(t, classifier.classify, path)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	captureQueued(t, bot, db)
	// Archived directly, since /topic archive arrives in week 3.
	_, err := openFile(t, path).ExecContext(ctx, `UPDATE topics SET archived_at = created_at WHERE name = 'running'`)
	if err != nil {
		t.Fatalf("archiving running: %v", err)
	}

	classifier.candidates = []cermin.TopicCandidate{
		{Name: "Running", Confidence: 0.95},
		{Name: "health", Confidence: 0.4},
	}
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id": messageID + 1,
		"text":       "Running shoes wear out after 800 km.",
	}))
	thought := captureQueued(t, bot, db)

	// The archived Topic is still stored as a candidate, and the best of the
	// others is kept in its place, even below the threshold.
	if sent := api.sentMessages(); len(sent) != 2 || sent[1].Text != "💡 idea · health" {
		t.Errorf("sent %+v, want a second Acknowledgement 💡 idea · health", sent)
	}
	classifications, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want 1", classifications, err)
	}
	wantStored := []cermin.TopicCandidate{
		{Name: "running", Confidence: 0.95},
		{Name: "health", Confidence: 0.4, Kept: true},
	}
	if got := classifications[0].Topics; !slices.Equal(got, wantStored) {
		t.Errorf("stored candidates %+v, want %+v", got, wantStored)
	}
}

func TestBot_ProcessInbox_SavesWhenClassificationTimesOut(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	timesOut := func(ctx context.Context, _ string, _ []cermin.Kind, _ []string) (cermin.Classification, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Errorf("classifying with deadline %v (set: %t), want one at most 8s away", deadline, ok)
		}
		return cermin.Classification{}, context.DeadlineExceeded
	}
	bot, db, api := newBot(t, timesOut)
	api.queue(textUpdate(t, ownerID, "Why does Go still not have sum types?", 0))

	thought := captureQueued(t, bot, db)

	wantSent := []sentMessage{{ChatID: ownerID, Text: "✓ saved", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	if got, err := db.GetClassificationsByThoughtID(ctx, thought.ID); err != nil || len(got) != 0 {
		t.Errorf("GetClassificationsByThoughtID = %+v, %v; want none", got, err)
	}
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
}

// A Classification that arrives as Capture's context ends, at its 8 s
// timeout or at shutdown, is saved, so that the catch-up doesn't pay for it
// again.
func TestBot_Process_SavesClassificationArrivingAsCaptureEnds(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	captureCtx, endCapture := context.WithCancel(ctx)
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, error) {
		endCapture()
		return classifier.classify(ctx, text, kinds, topics)
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	in := pollUpdate(t, bot, db)

	// With its context ended, the Capture can't send the Acknowledgement or
	// mark the update processed.
	if err := bot.Process(captureCtx, in); err == nil {
		t.Fatal("Process with its context ended succeeded, want it to fail")
	}
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	if calls := len(classifier.gotTopics); calls != 1 {
		t.Errorf("classified %d times, want once", calls)
	}
	thoughts, err := db.GetThoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("GetThoughts = %+v, %v; want 1", thoughts, err)
	}
	if got, err := db.GetClassificationsByThoughtID(ctx, thoughts[0].ID); err != nil || len(got) != 1 {
		t.Errorf("GetClassificationsByThoughtID = %+v, %v; want 1", got, err)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
}

func TestBot_ProcessInbox_RetryKeepsClassification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.failNextSend()
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	processQueued(t, bot)

	// The failed Acknowledgement holds up no update behind it.
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}

	if calls := len(classifier.gotTopics); calls != 1 {
		t.Errorf("classified %d times, want once", calls)
	}
	thoughts, err := db.GetThoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("GetThoughts = %+v, %v; want 1", thoughts, err)
	}
	if got, err := db.GetClassificationsByThoughtID(ctx, thoughts[0].ID); err != nil || len(got) != 1 {
		t.Errorf("GetClassificationsByThoughtID = %+v, %v; want 1", got, err)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
}

func TestBot_ProcessInbox_IgnoresOtherChats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		update string
	}{
		{
			name:   "text from someone else",
			update: textUpdate(t, ownerID+1, "Hi, is this bot public?", 0),
		},
		{
			name:   "command from someone else",
			update: textUpdate(t, ownerID+1, "/start", 0),
		},
		{
			name: "edit from someone else",
			update: update(t, "edited_message", ownerID+1, map[string]any{
				"text":      "Hi, is this bot private?",
				"edit_date": thoughtTime.Unix() + 60,
			}),
		},
		{
			name: "photo from someone else",
			update: update(t, "message", ownerID+1, map[string]any{
				"photo": []map[string]any{{"file_id": "a", "file_unique_id": "b", "width": 90, "height": 90}},
			}),
		},
		{
			// Message IDs are unique only within a chat, so a group message
			// could share its ID with a captured Thought.
			name: "owner's message in a group",
			update: update(t, "message", ownerID, map[string]any{
				"chat": map[string]any{"id": -1001, "type": "group"},
				"text": "Cafés near the office are full at 3pm.",
			}),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{}
			bot, db, api := newBot(t, classifier.classify)
			api.queue(tt.update)

			if err := bot.Poll(ctx); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if err := bot.ProcessInbox(ctx); err != nil {
				t.Fatalf("ProcessInbox: %v", err)
			}

			if thoughts, err := db.GetThoughts(ctx); err != nil || len(thoughts) != 0 {
				t.Errorf("GetThoughts = %+v, %v; want none", thoughts, err)
			}
			if sent := api.sentMessages(); len(sent) != 0 {
				t.Errorf("sent %+v, want nothing", sent)
			}
			if calls := len(classifier.gotTopics); calls != 0 {
				t.Errorf("classified %d times, want never", calls)
			}
			if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_ProcessInbox_RepliesWithoutCapturing(t *testing.T) {
	t.Parallel()
	const helpReply = "Send any text to save it as a Thought, unless it starts with /.\n\n" +
		"Reply to a Thought or its Acknowledgement with:\n" +
		"/kind <kind> to correct its Kind\n" +
		"/topics <a>, <b>, <c> to replace its 1 to 3 Topics\n" +
		"/delete to delete it"
	tests := []struct {
		name        string
		update      string
		wantReply   string
		wantReplyTo int64
	}{
		{
			name: "photo with a caption",
			update: update(t, "message", ownerID, map[string]any{
				"message_id": commandID,
				"photo":      []map[string]any{{"file_id": "a", "file_unique_id": "b", "width": 90, "height": 90}},
				"caption":    "The bus stop queue this morning.",
			}),
			wantReply:   "Text only for now.",
			wantReplyTo: commandID,
		},
		{
			name: "edit of the thought",
			update: withUpdateID(t, update(t, "edited_message", ownerID, map[string]any{
				"text":      "Start running on Sundays.",
				"edit_date": thoughtTime.Unix() + 60,
			}), commandID),
			wantReply:   "Edits aren't saved. Send it as a new Thought.",
			wantReplyTo: messageID,
		},
		{
			name:        "start",
			update:      commandUpdate(t, commandID, "/start", 0),
			wantReply:   helpReply,
			wantReplyTo: commandID,
		},
		{
			name:        "help",
			update:      commandUpdate(t, commandID, "/help", 0),
			wantReply:   helpReply,
			wantReplyTo: commandID,
		},
		{
			name:        "unknown command",
			update:      commandUpdate(t, commandID, "/remind tomorrow", 0),
			wantReply:   "Unknown command.\n\n" + helpReply,
			wantReplyTo: commandID,
		},
		{
			name:        "text starting with a slash",
			update:      commandUpdate(t, commandID, "/r/running says to start slow.", 0),
			wantReply:   "Unknown command.\n\n" + helpReply,
			wantReplyTo: commandID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
			path := filepath.Join(t.TempDir(), "cermin.db")
			bot, db, api := newBotAt(t, classifier.classify, path)
			file := openFile(t, path)
			api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
			thoughts := []cermin.Thought{captureQueued(t, bot, db)}

			api.queue(tt.update)
			processQueued(t, bot)

			sent := api.sentMessages()
			wantReply := sentMessage{ChatID: ownerID, Text: tt.wantReply, ReplyToID: tt.wantReplyTo}
			if len(sent) != 2 || sent[1] != wantReply {
				t.Errorf("sent %+v, want the Acknowledgement and then %+v", sent, wantReply)
			}
			if got, err := db.GetThoughts(ctx); err != nil || !reflect.DeepEqual(got, thoughts) {
				t.Errorf("GetThoughts = %+v, %v; want them unchanged: %+v", got, err, thoughts)
			}
			if calls := len(classifier.gotTopics); calls != 1 {
				t.Errorf("classified %d times, want once, for the Thought", calls)
			}
			// The raw update stays in the inbox, processed.
			if got := updateIDs(t, file); !slices.Equal(got, []int64{messageID, commandID}) {
				t.Errorf("inbox updates = %v, want %v", got, []int64{messageID, commandID})
			}
			if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_ProcessInbox_RepliesOncePerAlbumAndNeverToServiceMessages(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	captureQueued(t, bot, db)

	photo := []map[string]any{{"file_id": "a", "file_unique_id": "b", "width": 90, "height": 90}}
	for i := range 2 {
		api.queue(update(t, "message", ownerID, map[string]any{
			"message_id":     commandID + i,
			"media_group_id": "13",
			"photo":          photo,
		}))
	}
	// Service messages you can cause in a private chat with a bot.
	services := []map[string]any{
		{"pinned_message": repliedMessage(ownerID, messageID)},
		{"message_auto_delete_timer_changed": map[string]any{"message_auto_delete_time": 86400}},
		{"chat_background_set": map[string]any{"type": map[string]any{"type": "chat_theme", "theme_name": "🌷"}}},
	}
	for i, fields := range services {
		fields["message_id"] = commandID + 2 + i
		api.queue(update(t, "message", ownerID, fields))
	}
	processQueued(t, bot)

	wantReply := sentMessage{ChatID: ownerID, Text: "Text only for now.", ReplyToID: commandID}
	if sent := api.sentMessages(); len(sent) != 2 || sent[1] != wantReply {
		t.Errorf("sent %+v, want the Acknowledgement and then only %+v", sent, wantReply)
	}
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
}

func TestBot_Process_SafeToRepeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	in := pollUpdate(t, bot, db)

	// After a crash before the update was marked processed, a restart
	// processes it again.
	for range 2 {
		if err := bot.Process(ctx, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	if thoughts, err := db.GetThoughts(ctx); err != nil || len(thoughts) != 1 {
		t.Errorf("GetThoughts = %+v, %v; want 1", thoughts, err)
	}
	if sent := api.sentMessages(); len(sent) != 1 {
		t.Errorf("sent %+v, want 1 Acknowledgement", sent)
	}
}
