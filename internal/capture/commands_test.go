package capture_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

func TestBot_ProcessInbox_CorrectsKindKeepingTopics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		replyToID int64
	}{
		{name: "reply to the thought", replyToID: messageID},
		{name: "reply to its acknowledgement", replyToID: ackID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{
				{Name: "transport", Confidence: 0.95},
				{Name: "cities", Confidence: 0.8},
				{Name: "design", Confidence: 0.4},
			}}
			bot, db, api := newBot(t, classifier.classify)
			api.queue(textUpdate(t, ownerID, "What if bus stops showed how full the next bus is?", 0))
			thought := captureQueued(t, bot, db)
			before, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
			if err != nil || len(before) != 1 {
				t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want 1", before, err)
			}

			api.queue(commandUpdate(t, commandID, "/kind plan", tt.replyToID))
			processQueued(t, bot)

			after, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
			if err != nil || len(after) != 2 {
				t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want Cermin's and the Correction", after, err)
			}
			if !reflect.DeepEqual(after[0], before[0]) {
				t.Errorf("Cermin's Classification = %+v, want it unchanged: %+v", after[0], before[0])
			}
			got := after[1]
			got.ID, got.CreatedAt = 0, time.Time{}
			want := cermin.Classification{
				ThoughtID: thought.ID,
				Source:    cermin.SourceCorrection,
				Kind:      "plan",
				Topics: []cermin.TopicCandidate{
					{Name: "transport", Kept: true},
					{Name: "cities", Kept: true},
				},
				TGMessageID: commandID,
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Correction = %+v, want %+v", got, want)
			}
			wantEdits := []editedMessage{
				{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · transport, cities"},
			}
			if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
				t.Errorf("edited %+v, want %+v", edits, wantEdits)
			}
			if sent := api.sentMessages(); len(sent) != 1 {
				t.Errorf("sent %+v, want only the Acknowledgement", sent)
			}
			if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_ProcessInbox_CorrectsTopicsKeepingKind(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{
		{Name: "transport", Confidence: 0.95},
		{Name: "cities", Confidence: 0.8},
	}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "What if bus stops showed how full the next bus is?", 0))
	thought := captureQueued(t, bot, db)

	api.queue(commandUpdate(t, commandID, "/topics  Cities, Bus Stops ", messageID))
	processQueued(t, bot)
	// The current Classification is now the Correction above, so this one
	// copies that Correction's Topics.
	api.queue(commandUpdate(t, commandID+1, "/kind plan", ackID))
	processQueued(t, bot)

	classifications, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
	if err != nil || len(classifications) != 3 {
		t.Fatalf("GetClassificationsByThoughtID = %+v, %v; want Cermin's and 2 Corrections", classifications, err)
	}
	got := classifications[1:]
	for i := range got {
		got[i].ID, got[i].CreatedAt = 0, time.Time{}
	}
	correctedTopics := []cermin.TopicCandidate{{Name: "cities", Kept: true}, {Name: "bus stops", Kept: true}}
	want := []cermin.Classification{
		{
			ThoughtID:   thought.ID,
			Source:      cermin.SourceCorrection,
			Kind:        "idea",
			Topics:      correctedTopics,
			TGMessageID: commandID,
		},
		{
			ThoughtID:   thought.ID,
			Source:      cermin.SourceCorrection,
			Kind:        "plan",
			Topics:      correctedTopics,
			TGMessageID: commandID + 1,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Corrections = %+v, want %+v", got, want)
	}
	wantEdits := []editedMessage{
		{ChatID: ownerID, MessageID: ackID, Text: "✏️ 💡 idea · cities, bus stops"},
		{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · cities, bus stops"},
	}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
	wantTopics := []string{"bus stops", "cities", "transport"}
	if got, err := db.GetTopicNames(ctx); err != nil || !slices.Equal(got, wantTopics) {
		t.Errorf("GetTopicNames = %q, %v; want %q", got, err, wantTopics)
	}
}

func TestBot_ProcessInbox_RejectsCorrection(t *testing.T) {
	t.Parallel()
	const (
		wantKindUsage = "Reply to a Thought or its Acknowledgement with /kind and one of: " +
			"idea, question, observation, reflection, plan."
		wantTopicsUsage = "Reply to a Thought or its Acknowledgement with /topics and 1 to 3 Topics, " +
			"separated by commas."
	)
	tests := []struct {
		name          string
		classifierErr error
		command       string
		replyToID     int64
		wantReply     string
	}{
		{name: "kind not a reply", command: "/kind plan", wantReply: wantKindUsage},
		{name: "topics not a reply", command: "/topics running", wantReply: wantTopicsUsage},
		{name: "reply to another message", command: "/kind plan", replyToID: 999, wantReply: wantKindUsage},
		{name: "unknown kind", command: "/kind rant", replyToID: ackID, wantReply: wantKindUsage},
		{name: "no kind", command: "/kind", replyToID: ackID, wantReply: wantKindUsage},
		{name: "no topics", command: "/topics , ", replyToID: ackID, wantReply: wantTopicsUsage},
		{
			name:      "4 topics",
			command:   "/topics running, health, weekend, habits",
			replyToID: ackID,
			wantReply: wantTopicsUsage,
		},
		{
			name:          "thought not classified yet",
			classifierErr: errors.New("classifier down"),
			command:       "/kind plan",
			replyToID:     messageID,
			wantReply:     "Kind and Topics aren't ready yet. Try again in a minute.",
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
			api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
			thought := captureQueued(t, bot, db)
			before, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
			if err != nil {
				t.Fatalf("GetClassificationsByThoughtID: %v", err)
			}

			api.queue(commandUpdate(t, commandID, tt.command, tt.replyToID))
			processQueued(t, bot)

			sent := api.sentMessages()
			wantReply := sentMessage{ChatID: ownerID, Text: tt.wantReply, ReplyToID: commandID}
			if len(sent) != 2 || sent[1] != wantReply {
				t.Errorf("sent %+v, want the Acknowledgement and then %+v", sent, wantReply)
			}
			if edits := api.editedMessages(); len(edits) != 0 {
				t.Errorf("edited %+v, want nothing", edits)
			}
			after, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
			if err != nil || len(after) != len(before) {
				t.Errorf("GetClassificationsByThoughtID = %+v, %v; want %d as before", after, err, len(before))
			}
			if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_Process_CorrectionSafeToRepeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)
	api.queue(commandUpdate(t, commandID, "/kind plan", ackID))
	in := pollUpdate(t, bot, db)

	// After a crash before the update was marked processed, a restart
	// processes it again.
	for range 2 {
		if err := bot.Process(ctx, in); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	if got, err := db.GetClassificationsByThoughtID(ctx, thought.ID); err != nil || len(got) != 2 {
		t.Errorf("GetClassificationsByThoughtID = %+v, %v; want Cermin's and one Correction", got, err)
	}
	// The second run finds the Acknowledgement showing it already.
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_ProcessInbox_KeepsCorrectionWhenEditFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	api.failNextEdit()
	api.queue(commandUpdate(t, commandID, "/kind plan", messageID))
	processQueued(t, bot)

	if edits := api.editedMessages(); len(edits) != 0 {
		t.Fatalf("edited %+v, want the edit failed", edits)
	}
	classifications, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
	if err != nil || len(classifications) != 2 || classifications[1].Kind != "plan" {
		t.Errorf(
			"GetClassificationsByThoughtID = %+v, %v; want Cermin's and the Correction to plan",
			classifications,
			err,
		)
	}
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none, so nothing waits behind the Correction", updates, err)
	}

	if err := bot.CatchUp(ctx); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · running"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("after CatchUp, edited %+v, want %+v", edits, wantEdits)
	}
}

func TestBot_ProcessInbox_DeletesThought(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		replyToID int64
	}{
		{name: "reply to the thought", replyToID: messageID},
		{name: "reply to its acknowledgement", replyToID: ackID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "transport", Confidence: 0.9}}}
			path := filepath.Join(t.TempDir(), "cermin.db")
			bot, db, api := newBotAt(t, classifier.classify, path)
			file := openFile(t, path)
			// The Thought has the text every reply below carries. Another
			// Thought, captured next, is left as it is.
			api.queue(textUpdate(t, ownerID, repliedText, 0))
			thought := captureQueued(t, bot, db)
			api.queue(update(t, "message", ownerID, map[string]any{
				"message_id": messageID + 1,
				"text":       "Cafés near the office are full at 3pm.",
			}))
			// As Telegram sends them, a reply to the Thought, which is a
			// Thought of its own, and a Correction, which replies to its
			// Acknowledgement, carry a copy of the message they reply to, and
			// your edit of the Thought's message carries its text.
			api.queue(update(t, "message", ownerID, map[string]any{
				"message_id":       messageID + 2,
				"text":             "Maybe leave at 7am instead.",
				"reply_to_message": repliedMessage(ownerID, messageID),
			}))
			edit := update(t, "edited_message", ownerID, map[string]any{
				"text":      repliedText + " Even on Fridays.",
				"edit_date": thoughtTime.Unix() + 60,
			})
			api.queue(withUpdateID(t, edit, messageID+3))
			api.queue(commandUpdate(t, commandID, "/kind observation", ackID))
			processQueued(t, bot)

			thoughts, err := db.GetThoughts(ctx)
			if err != nil || len(thoughts) != 3 {
				t.Fatalf("GetThoughts = %+v, %v; want 3", thoughts, err)
			}
			others := thoughts[1:]
			othersClassifications := classificationsOf(t, db, others)
			wantColumns := []string{"telegram_updates.payload", "thoughts.text"}
			if got := columnsContaining(t, file, repliedText); !slices.Equal(got, wantColumns) {
				t.Fatalf("before /delete, the Thought's text is in %q, want %q", got, wantColumns)
			}

			api.queue(commandUpdate(t, commandID+1, "/delete", tt.replyToID))
			processQueued(t, bot)

			if got := columnsContaining(t, file, repliedText); len(got) != 0 {
				t.Errorf("after /delete, the Thought's text is in %q, want nowhere", got)
			}
			if got, err := db.GetClassificationsByThoughtID(ctx, thought.ID); err != nil || len(got) != 0 {
				t.Errorf("GetClassificationsByThoughtID of the deleted Thought = %+v, %v; want none", got, err)
			}
			var orphans int
			err = file.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM classification_topics
				WHERE classification_id NOT IN (SELECT id FROM classifications)`,
			).Scan(&orphans)
			if err != nil || orphans != 0 {
				t.Errorf("%d candidate Topics left without their Classification, %v; want none", orphans, err)
			}
			if got, err := db.GetThoughts(ctx); err != nil || !reflect.DeepEqual(got, others) {
				t.Errorf("GetThoughts = %+v, %v; want the others unchanged: %+v", got, err, others)
			}
			if got := classificationsOf(t, db, others); !reflect.DeepEqual(got, othersClassifications) {
				t.Errorf("Classifications of the others = %+v, want them unchanged: %+v", got, othersClassifications)
			}
			// The reply's and the Correction's updates stay, keeping only the
			// ID of the message they reply to.
			want := []int64{messageID + 1, messageID + 2, commandID}
			if got := updateIDs(t, file); !slices.Equal(got, want) {
				t.Errorf("inbox updates = %v, want the other Thoughts' and the Correction's: %v", got, want)
			}
			wantEdits := []editedMessage{
				{ChatID: ownerID, MessageID: ackID, Text: "✏️ 👀 observation · transport"},
				{ChatID: ownerID, MessageID: ackID, Text: "🗑 deleted"},
			}
			if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
				t.Errorf("edited %+v, want %+v", edits, wantEdits)
			}
			if sent := api.sentMessages(); len(sent) != 4 {
				t.Errorf("sent %+v, want only the 3 Acknowledgements and the reply to the edit", sent)
			}
		})
	}
}

func TestBot_ProcessInbox_KeepsDeletedThoughtsTextOut(t *testing.T) {
	t.Parallel()
	const editReply = "Edits aren't saved. Send it as a new Thought."
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "transport", Confidence: 0.9}}}
	path := filepath.Join(t.TempDir(), "cermin.db")
	bot, db, api := newBotAt(t, classifier.classify, path)
	file := openFile(t, path)
	api.queue(textUpdate(t, ownerID, repliedText, 0))
	captureQueued(t, bot, db)
	// Your pin of the Thought's message carries a copy of it.
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id":     messageID + 1,
		"pinned_message": repliedMessage(ownerID, messageID),
	}))
	api.queue(commandUpdate(t, commandID, "/delete", messageID))
	processQueued(t, bot)

	// After the /delete, a reply to the deleted Thought carries a copy of it,
	// and your edit of its message carries its text.
	api.queue(commandUpdate(t, commandID+1, "/kind plan", messageID))
	edit := update(t, "edited_message", ownerID, map[string]any{
		"text":      repliedText + " Even on Fridays.",
		"edit_date": thoughtTime.Unix() + 60,
	})
	api.queue(withUpdateID(t, edit, commandID+2))
	processQueued(t, bot)

	if got := columnsContaining(t, file, repliedText); len(got) != 0 {
		t.Errorf("the deleted Thought's text is in %q, want nowhere", got)
	}
	wantReply := sentMessage{ChatID: ownerID, Text: editReply, ReplyToID: messageID}
	if sent := api.sentMessages(); len(sent) == 0 || sent[len(sent)-1] != wantReply {
		t.Errorf("sent %+v, want the last to be %+v", sent, wantReply)
	}
	if updates, err := db.GetUnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
}

func TestBot_ProcessInbox_RejectsDelete(t *testing.T) {
	t.Parallel()
	const deleteUsage = "Reply to a Thought or its Acknowledgement with /delete."
	tests := []struct {
		name           string
		isDeletedFirst bool // the Thought is deleted before the /delete under test
		replyToID      int64
	}{
		{name: "not a reply"},
		{name: "reply to another message", replyToID: 999},
		{name: "reply to a deleted thought", isDeletedFirst: true, replyToID: messageID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "transport", Confidence: 0.9}}}
			path := filepath.Join(t.TempDir(), "cermin.db")
			bot, db, api := newBotAt(t, classifier.classify, path)
			file := openFile(t, path)
			api.queue(textUpdate(t, ownerID, repliedText, 0))
			captureQueued(t, bot, db)
			if tt.isDeletedFirst {
				api.queue(commandUpdate(t, commandID, "/delete", messageID))
				processQueued(t, bot)
			}
			thoughts, err := db.GetThoughts(ctx)
			if err != nil {
				t.Fatalf("GetThoughts: %v", err)
			}
			updates := updateIDs(t, file)
			edits := api.editedMessages()

			api.queue(commandUpdate(t, commandID+1, "/delete", tt.replyToID))
			processQueued(t, bot)

			sent := api.sentMessages()
			wantReply := sentMessage{ChatID: ownerID, Text: deleteUsage, ReplyToID: commandID + 1}
			if len(sent) != 2 || sent[1] != wantReply {
				t.Errorf("sent %+v, want the Acknowledgement and then %+v", sent, wantReply)
			}
			if got, err := db.GetThoughts(ctx); err != nil || !reflect.DeepEqual(got, thoughts) {
				t.Errorf("GetThoughts = %+v, %v; want them unchanged: %+v", got, err, thoughts)
			}
			// The /delete's own update stays, processed: it keeps only the ID
			// of the message it replies to.
			if got, want := updateIDs(t, file), append(updates, commandID+1); !slices.Equal(got, want) {
				t.Errorf("inbox updates = %v, want %v", got, want)
			}
			if unprocessed, err := db.GetUnprocessedUpdates(ctx); err != nil || len(unprocessed) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", unprocessed, err)
			}
			if got := api.editedMessages(); !slices.Equal(got, edits) {
				t.Errorf("edited %+v, want %+v as before", got, edits)
			}
		})
	}
}
