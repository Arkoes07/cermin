package capture_test

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	botToken  = "123456:test-token"
	ownerID   = 42
	messageID = 7    // the Telegram message carrying the Thought
	ackID     = 1001 // its Acknowledgement, the first message the fake Bot API sends
	commandID = 20   // the Telegram message carrying a command
)

// thoughtTime is when the test Thoughts were sent: 08:30 WIB.
var thoughtTime = time.Date(2026, 10, 12, 1, 30, 0, 0, time.UTC)

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

	thoughts, err := db.Thoughts(ctx)
	if err != nil {
		t.Fatalf("Thoughts: %v", err)
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
		ID:                 got.ID,
		Text:               "What if bus stops showed how full the next bus is?",
		Source:             "telegram",
		TGMessageID:        messageID,
		TGReplyToMessageID: 5,
		AckMessageID:       1001,
		Language:           "en",
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

	classifications, err := db.Classifications(ctx, got.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("Classifications = %+v, %v; want 1", classifications, err)
	}
	gotClassification := classifications[0]
	if gotClassification.CreatedAt.Before(before) || gotClassification.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", gotClassification.CreatedAt, before, after)
	}
	gotClassification.ID, gotClassification.CreatedAt = 0, time.Time{}
	wantClassification := cermin.Classification{
		ThoughtID:      got.ID,
		Source:         "cermin",
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
	}
	if !reflect.DeepEqual(gotClassification, wantClassification) {
		t.Errorf("Classification = %+v, want %+v", gotClassification, wantClassification)
	}
}

func TestBot_ProcessInbox_KeepsStrongTopics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		candidates []cermin.TopicCandidate
		wantAck    string
		wantStored []cermin.TopicCandidate // every candidate, best first
		wantTopics []string                // the Topics that exist afterwards
	}{
		{
			name: "those at 0.7 or above, best first",
			candidates: []cermin.TopicCandidate{
				{Name: "cities", Confidence: 0.7},
				{Name: "transport", Confidence: 0.95},
				{Name: "design", Confidence: 0.4},
			},
			wantAck: "💡 idea · transport, cities",
			wantStored: []cermin.TopicCandidate{
				{Name: "transport", Confidence: 0.95, Kept: true},
				{Name: "cities", Confidence: 0.7, Kept: true},
				{Name: "design", Confidence: 0.4},
			},
			wantTopics: []string{"cities", "transport"},
		},
		{
			name: "at most 3",
			candidates: []cermin.TopicCandidate{
				{Name: "data", Confidence: 0.72},
				{Name: "transport", Confidence: 0.95},
				{Name: "cities", Confidence: 0.8},
				{Name: "design", Confidence: 0.75},
			},
			wantAck: "💡 idea · transport, cities, design",
			wantStored: []cermin.TopicCandidate{
				{Name: "transport", Confidence: 0.95, Kept: true},
				{Name: "cities", Confidence: 0.8, Kept: true},
				{Name: "design", Confidence: 0.75, Kept: true},
				{Name: "data", Confidence: 0.72},
			},
			wantTopics: []string{"cities", "design", "transport"},
		},
		{
			name: "the single best when none reaches 0.7",
			candidates: []cermin.TopicCandidate{
				{Name: "design", Confidence: 0.5},
				{Name: "cities", Confidence: 0.6},
				{Name: "data", Confidence: 0.3},
			},
			wantAck: "💡 idea · cities",
			wantStored: []cermin.TopicCandidate{
				{Name: "cities", Confidence: 0.6, Kept: true},
				{Name: "design", Confidence: 0.5},
				{Name: "data", Confidence: 0.3},
			},
			wantTopics: []string{"cities"},
		},
		{
			name: "names lowercased, blank and repeated ones dropped",
			candidates: []cermin.TopicCandidate{
				{Name: " Transport ", Confidence: 0.9},
				{Name: "", Confidence: 0.99},
				{Name: "transport", Confidence: 0.8},
				{Name: "Cities", Confidence: 0.75},
			},
			wantAck: "💡 idea · transport, cities",
			wantStored: []cermin.TopicCandidate{
				{Name: "transport", Confidence: 0.9, Kept: true},
				{Name: "cities", Confidence: 0.75, Kept: true},
			},
			wantTopics: []string{"cities", "transport"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			classifier := &fakeClassifier{candidates: tt.candidates}
			bot, db, api := newBot(t, classifier.classify)
			api.queue(textUpdate(t, ownerID, "What if bus stops showed how full the next bus is?", 0))

			thought := captureQueued(t, bot, db)

			wantSent := []sentMessage{{ChatID: ownerID, Text: tt.wantAck, ReplyToID: messageID}}
			if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
				t.Errorf("sent %+v, want %+v", sent, wantSent)
			}
			classifications, err := db.Classifications(ctx, thought.ID)
			if err != nil || len(classifications) != 1 {
				t.Fatalf("Classifications = %+v, %v; want 1", classifications, err)
			}
			if got := classifications[0].Topics; !slices.Equal(got, tt.wantStored) {
				t.Errorf("stored candidates %+v, want %+v", got, tt.wantStored)
			}
			if got, err := db.TopicNames(ctx); err != nil || !slices.Equal(got, tt.wantTopics) {
				t.Errorf("TopicNames = %q, %v; want %q", got, err, tt.wantTopics)
			}
		})
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
	classifications, err := db.Classifications(ctx, thought.ID)
	if err != nil || len(classifications) != 1 {
		t.Fatalf("Classifications = %+v, %v; want 1", classifications, err)
	}
	wantStored := []cermin.TopicCandidate{
		{Name: "business", Confidence: 0.85, Kept: true},
		{Name: "retail", Confidence: 0.8, Kept: true},
	}
	if got := classifications[0].Topics; !slices.Equal(got, wantStored) {
		t.Errorf("stored candidates %+v, want %+v", got, wantStored)
	}
	if got, err := db.TopicNames(ctx); err != nil || !slices.Equal(got, []string{"business", "retail"}) {
		t.Errorf("TopicNames = %q, %v; want business, retail", got, err)
	}
}

func TestBot_ProcessInbox_SavesWhenClassificationTimesOut(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	timesOut := func(ctx context.Context, _ string, _ []cermin.Kind, _ []string) (cermin.Classification, string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 8*time.Second {
			t.Errorf("classifying with deadline %v (set: %t), want one at most 8s away", deadline, ok)
		}
		return cermin.Classification{}, "", context.DeadlineExceeded
	}
	bot, db, api := newBot(t, timesOut)
	api.queue(textUpdate(t, ownerID, "Why does Go still not have sum types?", 0))

	thought := captureQueued(t, bot, db)

	wantSent := []sentMessage{{ChatID: ownerID, Text: "✓ saved", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	if classifications, err := db.Classifications(ctx, thought.ID); err != nil || len(classifications) != 0 {
		t.Errorf("Classifications = %+v, %v; want none", classifications, err)
	}
	if thought.Language != "" {
		t.Errorf("Language = %q, want none until classified", thought.Language)
	}
	if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
	}
}

func TestBot_ProcessInbox_RetryKeepsClassification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.failNextSend()
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}

	if err := bot.ProcessInbox(ctx); err == nil {
		t.Fatal("ProcessInbox succeeded although the Acknowledgement failed to send")
	}
	if err := bot.ProcessInbox(ctx); err != nil {
		t.Fatalf("ProcessInbox again: %v", err)
	}

	if calls := len(classifier.gotTopics); calls != 1 {
		t.Errorf("classified %d times, want once", calls)
	}
	thoughts, err := db.Thoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("Thoughts = %+v, %v; want 1", thoughts, err)
	}
	if classifications, err := db.Classifications(ctx, thoughts[0].ID); err != nil || len(classifications) != 1 {
		t.Errorf("Classifications = %+v, %v; want 1", classifications, err)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
}

func TestBot_ProcessInbox_CapturesOnlyOwnerText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		update string
	}{
		{
			name:   "message from someone else",
			update: textUpdate(t, ownerID+1, "Hi, is this bot public?", 0),
		},
		{
			name:   "owner's command",
			update: textUpdate(t, ownerID, "/start", 0),
		},
		{
			name: "owner's edit",
			update: update(t, "edited_message", ownerID, map[string]any{
				"text":      "Start running on Saturdays.",
				"edit_date": thoughtTime.Unix() + 60,
			}),
		},
		{
			name: "owner's photo",
			update: update(t, "message", ownerID, map[string]any{
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

			if thoughts, err := db.Thoughts(ctx); err != nil || len(thoughts) != 0 {
				t.Errorf("Thoughts = %+v, %v; want none", thoughts, err)
			}
			if sent := api.sentMessages(); len(sent) != 0 {
				t.Errorf("sent %+v, want nothing", sent)
			}
			if calls := len(classifier.gotTopics); calls != 0 {
				t.Errorf("classified %d times, want never", calls)
			}
			if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_Process_SafeToRepeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	updates, err := db.UnprocessedUpdates(ctx)
	if err != nil || len(updates) != 1 {
		t.Fatalf("unprocessed updates = %+v, %v; want 1", updates, err)
	}

	// After a crash before the update was marked processed, a restart
	// processes it again.
	for range 2 {
		if err := bot.Process(ctx, updates[0]); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	if thoughts, err := db.Thoughts(ctx); err != nil || len(thoughts) != 1 {
		t.Errorf("Thoughts = %+v, %v; want 1", thoughts, err)
	}
	if sent := api.sentMessages(); len(sent) != 1 {
		t.Errorf("sent %+v, want 1 Acknowledgement", sent)
	}
}

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
			before, err := db.Classifications(ctx, thought.ID)
			if err != nil || len(before) != 1 {
				t.Fatalf("Classifications = %+v, %v; want 1", before, err)
			}

			api.queue(commandUpdate(t, commandID, "/kind plan", tt.replyToID))
			processQueued(t, bot)

			after, err := db.Classifications(ctx, thought.ID)
			if err != nil || len(after) != 2 {
				t.Fatalf("Classifications = %+v, %v; want Cermin's and the Correction", after, err)
			}
			if !reflect.DeepEqual(after[0], before[0]) {
				t.Errorf("Cermin's Classification = %+v, want it unchanged: %+v", after[0], before[0])
			}
			got := after[1]
			got.ID, got.CreatedAt = 0, time.Time{}
			want := cermin.Classification{
				ThoughtID: thought.ID,
				Source:    "correction",
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
			if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
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

	classifications, err := db.Classifications(ctx, thought.ID)
	if err != nil || len(classifications) != 3 {
		t.Fatalf("Classifications = %+v, %v; want Cermin's and 2 Corrections", classifications, err)
	}
	got := classifications[1:]
	for i := range got {
		got[i].ID, got[i].CreatedAt = 0, time.Time{}
	}
	correctedTopics := []cermin.TopicCandidate{{Name: "cities", Kept: true}, {Name: "bus stops", Kept: true}}
	want := []cermin.Classification{
		{
			ThoughtID:   thought.ID,
			Source:      "correction",
			Kind:        "idea",
			Topics:      correctedTopics,
			TGMessageID: commandID,
		},
		{
			ThoughtID:   thought.ID,
			Source:      "correction",
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
	if got, err := db.TopicNames(ctx); err != nil || !slices.Equal(got, wantTopics) {
		t.Errorf("TopicNames = %q, %v; want %q", got, err, wantTopics)
	}
}

func TestBot_ProcessInbox_RejectsCorrection(t *testing.T) {
	t.Parallel()
	const (
		kindUsage = "Reply to a Thought or its Acknowledgement with /kind and one of: " +
			"idea, question, observation, reflection, plan."
		topicsUsage = "Reply to a Thought or its Acknowledgement with /topics and 1 to 3 Topics, " +
			"separated by commas."
	)
	tests := []struct {
		name          string
		classifierErr error
		command       string
		replyToID     int64
		wantReply     string
	}{
		{name: "kind not a reply", command: "/kind plan", wantReply: kindUsage},
		{name: "topics not a reply", command: "/topics running", wantReply: topicsUsage},
		{name: "reply to another message", command: "/kind plan", replyToID: 999, wantReply: kindUsage},
		{name: "unknown kind", command: "/kind rant", replyToID: ackID, wantReply: kindUsage},
		{name: "no kind", command: "/kind", replyToID: ackID, wantReply: kindUsage},
		{name: "no topics", command: "/topics , ", replyToID: ackID, wantReply: topicsUsage},
		{
			name:      "4 topics",
			command:   "/topics running, health, weekend, habits",
			replyToID: ackID,
			wantReply: topicsUsage,
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
			before, err := db.Classifications(ctx, thought.ID)
			if err != nil {
				t.Fatalf("Classifications: %v", err)
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
			if after, err := db.Classifications(ctx, thought.ID); err != nil || len(after) != len(before) {
				t.Errorf("Classifications = %+v, %v; want %d as before", after, err, len(before))
			}
			if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
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
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	updates, err := db.UnprocessedUpdates(ctx)
	if err != nil || len(updates) != 1 {
		t.Fatalf("unprocessed updates = %+v, %v; want 1", updates, err)
	}

	// After a crash before the update was marked processed, a restart
	// processes it again.
	for range 2 {
		if err := bot.Process(ctx, updates[0]); err != nil {
			t.Fatalf("Process: %v", err)
		}
	}

	if classifications, err := db.Classifications(ctx, thought.ID); err != nil || len(classifications) != 2 {
		t.Errorf("Classifications = %+v, %v; want Cermin's and one Correction", classifications, err)
	}
	// Both runs show it; Telegram leaves the second edit, which changes
	// nothing, as it is.
	edit := editedMessage{ChatID: ownerID, MessageID: ackID, Text: "✏️ 📋 plan · running"}
	if edits := api.editedMessages(); !slices.Equal(edits, []editedMessage{edit, edit}) {
		t.Errorf("edited %+v, want %+v twice", edits, edit)
	}
}

func TestBot_ProcessInbox_KeepsCorrectionWhenEditFails(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	classifier := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	bot, db, api := newBot(t, classifier.classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	thought := captureQueued(t, bot, db)

	// As when you deleted the Acknowledgement: no retry could edit it.
	api.failNextEdit()
	api.queue(commandUpdate(t, commandID, "/kind plan", messageID))
	processQueued(t, bot)

	if edits := api.editedMessages(); len(edits) != 0 {
		t.Fatalf("edited %+v, want the edit refused", edits)
	}
	classifications, err := db.Classifications(ctx, thought.ID)
	if err != nil || len(classifications) != 2 || classifications[1].Kind != "plan" {
		t.Errorf("Classifications = %+v, %v; want Cermin's and the Correction to plan", classifications, err)
	}
	if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
		t.Errorf("unprocessed updates = %+v, %v; want none, so nothing waits behind the Correction", updates, err)
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
			// A reply to the Thought, which is a Thought of its own, and your
			// edit of the Thought's message carry its text. A Correction
			// replies to its Acknowledgement.
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

			thoughts, err := db.Thoughts(ctx)
			if err != nil || len(thoughts) != 3 {
				t.Fatalf("Thoughts = %+v, %v; want 3", thoughts, err)
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
			if got, err := db.Classifications(ctx, thought.ID); err != nil || len(got) != 0 {
				t.Errorf("Classifications of the deleted Thought = %+v, %v; want none", got, err)
			}
			var orphans int
			err = file.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM classification_topics
				WHERE classification_id NOT IN (SELECT id FROM classifications)`,
			).Scan(&orphans)
			if err != nil || orphans != 0 {
				t.Errorf("%d candidate Topics left without their Classification, %v; want none", orphans, err)
			}
			if got, err := db.Thoughts(ctx); err != nil || !reflect.DeepEqual(got, others) {
				t.Errorf("Thoughts = %+v, %v; want the others unchanged: %+v", got, err, others)
			}
			if got := classificationsOf(t, db, others); !reflect.DeepEqual(got, othersClassifications) {
				t.Errorf("Classifications of the others = %+v, want them unchanged: %+v", got, othersClassifications)
			}
			// The reply's update goes too, since it carries the deleted
			// Thought's text, but the Thought it carried stays.
			if got, want := updateIDs(t, file), []int64{messageID + 1}; !slices.Equal(got, want) {
				t.Errorf("inbox updates = %v, want only the other Thought's: %v", got, want)
			}
			wantEdits := []editedMessage{
				{ChatID: ownerID, MessageID: ackID, Text: "✏️ 👀 observation · transport"},
				{ChatID: ownerID, MessageID: ackID, Text: "🗑 deleted"},
			}
			if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
				t.Errorf("edited %+v, want %+v", edits, wantEdits)
			}
			if sent := api.sentMessages(); len(sent) != 3 {
				t.Errorf("sent %+v, want only the 3 Acknowledgements", sent)
			}
		})
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
			thoughts, err := db.Thoughts(ctx)
			if err != nil {
				t.Fatalf("Thoughts: %v", err)
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
			if got, err := db.Thoughts(ctx); err != nil || !reflect.DeepEqual(got, thoughts) {
				t.Errorf("Thoughts = %+v, %v; want them unchanged: %+v", got, err, thoughts)
			}
			// The /delete's own update goes too: one that replies to a deleted
			// Thought carries its text.
			if got := updateIDs(t, file); !slices.Equal(got, updates) {
				t.Errorf("inbox updates = %v, want %v as before", got, updates)
			}
			if got := api.editedMessages(); !slices.Equal(got, edits) {
				t.Errorf("edited %+v, want %+v as before", got, edits)
			}
		})
	}
}

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
	) (cermin.Classification, string, error) {
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
			bot, _, api := newBot(t, classifier.classify)
			// A failed reply leaves what a crash before replying does: the
			// Thought saved without an Acknowledgement.
			api.failNextSend()
			api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
			if err := bot.Poll(ctx); err != nil {
				t.Fatalf("Poll: %v", err)
			}
			if err := bot.ProcessInbox(ctx); err == nil {
				t.Fatal("ProcessInbox succeeded although the Acknowledgement failed to send")
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

			// The inbox retries the update too, and must not send another.
			if err := bot.ProcessInbox(ctx); err != nil {
				t.Fatalf("ProcessInbox again: %v", err)
			}
			if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
				t.Errorf("after the inbox retry, sent %+v, want %+v", sent, wantSent)
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
			if err := bot.CatchUp(ctx); err != nil {
				t.Fatalf("CatchUp: %v", err)
			}
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
	for range 2 {
		if err := bot.CatchUp(ctx); err != nil {
			t.Fatalf("CatchUp: %v", err)
		}
	}
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

func TestBot_CatchUp_KeepsClassificationMadeMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	slow := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "errands", Confidence: 0.9}}}
	fast := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	slowStarted, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, string, error) {
		switch calls.Add(1) {
		case 1: // at Capture
			return cermin.Classification{}, "", errors.New("classifier down")
		case 2: // the catch-up's
			close(slowStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return cermin.Classification{}, "", ctx.Err()
			}
			return slow.classify(ctx, text, kinds, topics)
		default: // the inbox retry's
			return fast.classify(ctx, text, kinds, topics)
		}
	}
	bot, db, api := newBot(t, classify)
	api.failNextSend()
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := bot.ProcessInbox(ctx); err == nil {
		t.Fatal("ProcessInbox succeeded although the Acknowledgement failed to send")
	}

	// The catch-up's Acknowledgement fails too, then its classifier is slow.
	api.failNextSend()
	caughtUp := make(chan error, 1)
	go func() { caughtUp <- bot.CatchUp(ctx) }()
	<-slowStarted
	// Meanwhile the inbox retries the update, classifying the Thought itself.
	if err := bot.ProcessInbox(ctx); err != nil {
		t.Fatalf("ProcessInbox again: %v", err)
	}
	close(release)
	if err := <-caughtUp; err != nil {
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
		t.Errorf("kept the Classification with Topics %+v, want the inbox retry's %+v", got, wantTopics)
	}
	wantSent := []sentMessage{{ChatID: ownerID, Text: "💡 idea · running", ReplyToID: messageID}}
	if sent := api.sentMessages(); !slices.Equal(sent, wantSent) {
		t.Errorf("sent %+v, want %+v", sent, wantSent)
	}
	if edits := api.editedMessages(); len(edits) != 0 {
		t.Errorf("edited %+v, want nothing", edits)
	}
}

func TestBot_CatchUp_DropsClassificationOfThoughtDeletedMeanwhile(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	slow := &fakeClassifier{candidates: []cermin.TopicCandidate{{Name: "running", Confidence: 0.9}}}
	slowStarted, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	classify := func(
		ctx context.Context,
		text string,
		kinds []cermin.Kind,
		topics []string,
	) (cermin.Classification, string, error) {
		switch calls.Add(1) {
		case 2: // the catch-up's
			close(slowStarted)
			select {
			case <-release:
			case <-ctx.Done():
				return cermin.Classification{}, "", ctx.Err()
			}
			return slow.classify(ctx, text, kinds, topics)
		default: // at each Capture
			return cermin.Classification{}, "", errors.New("classifier down")
		}
	}
	bot, db, api := newBot(t, classify)
	api.queue(textUpdate(t, ownerID, "Start running on Saturdays.", 0))
	deleted := captureQueued(t, bot, db)

	caughtUp := make(chan error, 1)
	go func() { caughtUp <- bot.CatchUp(ctx) }()
	<-slowStarted
	// Meanwhile you delete the Thought and capture another, which the
	// classifier fails on too.
	api.queue(commandUpdate(t, commandID, "/delete", messageID))
	api.queue(update(t, "message", ownerID, map[string]any{
		"message_id": commandID + 1, // after the /delete
		"text":       "Buy new running shoes.",
	}))
	processQueued(t, bot)
	close(release)
	if err := <-caughtUp; err != nil {
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
	wantEdits := []editedMessage{{ChatID: ownerID, MessageID: ackID, Text: "🗑 deleted"}}
	if edits := api.editedMessages(); !slices.Equal(edits, wantEdits) {
		t.Errorf("edited %+v, want %+v", edits, wantEdits)
	}
}

func newBot(t *testing.T, classify capture.ClassifyFunc) (*capture.Bot, *sqlite.DB, *fakeBotAPI) {
	t.Helper()
	return newBotAt(t, classify, filepath.Join(t.TempDir(), "cermin.db"))
}

// newBotAt is newBot with its database at path.
func newBotAt(t *testing.T, classify capture.ClassifyFunc, path string) (*capture.Bot, *sqlite.DB, *fakeBotAPI) {
	t.Helper()
	api := &fakeBotAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)

	db, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})

	bot := capture.New(
		db,
		telegram.New(srv.URL, botToken),
		ownerID,
		classify,
	)
	return bot, db, api
}

// captureQueued polls the queued updates, processes the inbox and returns the
// latest Thought.
func captureQueued(t *testing.T, bot *capture.Bot, db *sqlite.DB) cermin.Thought {
	t.Helper()
	processQueued(t, bot)
	thoughts, err := db.Thoughts(t.Context())
	if err != nil || len(thoughts) == 0 {
		t.Fatalf("Thoughts = %+v, %v; want some", thoughts, err)
	}
	return thoughts[len(thoughts)-1]
}

// processQueued polls the queued updates and processes the inbox.
func processQueued(t *testing.T, bot *capture.Bot) {
	t.Helper()
	if err := bot.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := bot.ProcessInbox(t.Context()); err != nil {
		t.Fatalf("ProcessInbox: %v", err)
	}
}

// classificationsOf returns the Classifications of each of thoughts.
func classificationsOf(t *testing.T, db *sqlite.DB, thoughts []cermin.Thought) [][]cermin.Classification {
	t.Helper()
	all := make([][]cermin.Classification, 0, len(thoughts))
	for _, thought := range thoughts {
		classifications, err := db.Classifications(t.Context(), thought.ID)
		if err != nil {
			t.Fatalf("Classifications: %v", err)
		}
		all = append(all, classifications)
	}
	return all
}

// openFile opens the database file at path directly, with the driver package
// sqlite registers, to read what the store's methods don't show.
func openFile(t *testing.T, path string) *sql.DB {
	t.Helper()
	file, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	t.Cleanup(func() {
		if err := file.Close(); err != nil {
			t.Errorf("closing %s: %v", path, err)
		}
	})
	return file
}

// columnsContaining returns each column of every table in file, as
// "table.column", that has a value containing s.
func columnsContaining(t *testing.T, file *sql.DB, s string) []string {
	t.Helper()
	ctx := t.Context()
	rows, err := file.QueryContext(ctx, `
		SELECT m.name, c.name
		FROM sqlite_schema AS m, pragma_table_info(m.name) AS c
		WHERE m.type = 'table'
		ORDER BY m.name, c.cid`)
	if err != nil {
		t.Fatalf("listing columns: %v", err)
	}
	defer rows.Close()
	type column struct{ table, name string }
	columns := []column{}
	for rows.Next() {
		var c column
		if err := rows.Scan(&c.table, &c.name); err != nil {
			t.Fatalf("reading column: %v", err)
		}
		columns = append(columns, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing columns: %v", err)
	}

	found := []string{}
	for _, c := range columns {
		// The names come from the schema and are plain, so %q quotes them as
		// SQL would.
		query := fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE instr(CAST(%q AS TEXT), ?) > 0`, c.table, c.name)
		var n int
		if err := file.QueryRowContext(ctx, query, s).Scan(&n); err != nil {
			t.Fatalf("searching %s.%s: %v", c.table, c.name, err)
		}
		if n > 0 {
			found = append(found, c.table+"."+c.name)
		}
	}
	return found
}

// updateIDs returns the IDs of the updates in the inbox in file, processed or
// not, in order.
func updateIDs(t *testing.T, file *sql.DB) []int64 {
	t.Helper()
	rows, err := file.QueryContext(t.Context(), `SELECT update_id FROM telegram_updates ORDER BY update_id`)
	if err != nil {
		t.Fatalf("listing updates: %v", err)
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("reading update: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing updates: %v", err)
	}
	return ids
}

// fakeRawOutput is the raw model output of every fakeClassifier
// Classification.
const fakeRawOutput = `{"language":"en","kind":{"value":"idea","confidence":0.9}}`

// fakeClassifier classifies every Thought as English, of Kind kind (idea if
// unset), with its candidate Topics, or fails with err if set. It records the
// existing Topics it was given.
type fakeClassifier struct {
	kind       string
	candidates []cermin.TopicCandidate
	err        error
	gotTopics  [][]string // the existing Topics given to each call
}

func (f *fakeClassifier) classify(
	_ context.Context,
	_ string,
	_ []cermin.Kind,
	topics []string,
) (cermin.Classification, string, error) {
	f.gotTopics = append(f.gotTopics, topics)
	if f.err != nil {
		return cermin.Classification{}, "", f.err
	}
	c := cermin.Classification{
		Kind:             cmp.Or(f.kind, "idea"),
		KindConfidence:   0.9,
		Topics:           slices.Clone(f.candidates),
		Model:            "claude-test",
		PromptVersion:    "classify-v1",
		ConfidenceMethod: "verbalized-v1",
		RawOutput:        fakeRawOutput,
	}
	return c, "en", nil
}

// textUpdate is an update carrying a text message. A non-zero replyToID
// makes it a reply to that message.
func textUpdate(t *testing.T, fromID int64, text string, replyToID int64) string {
	t.Helper()
	fields := map[string]any{"text": text}
	if replyToID != 0 {
		fields["reply_to_message"] = repliedMessage(fromID, replyToID)
	}
	return update(t, "message", fromID, fields)
}

// commandUpdate is an update carrying the owner's command text in message id,
// in reply to message replyToID, or to none if replyToID is 0.
func commandUpdate(t *testing.T, id int64, text string, replyToID int64) string {
	t.Helper()
	fields := map[string]any{"message_id": id, "text": text}
	if replyToID != 0 {
		fields["reply_to_message"] = repliedMessage(ownerID, replyToID)
	}
	return update(t, "message", ownerID, fields)
}

// repliedText is the text of every message a test update replies to.
const repliedText = "Buses are always packed at 8am."

// repliedMessage is message id in the private chat of user chatID, reading
// repliedText, as a reply carries the message it replies to.
func repliedMessage(chatID, id int64) map[string]any {
	return map[string]any{
		"message_id": id,
		"chat":       map[string]any{"id": chatID, "type": "private"},
		"date":       thoughtTime.Add(-time.Hour).Unix(),
		"text":       repliedText,
	}
}

// update is a Telegram update whose field ("message" or "edited_message") is
// message messageID from fromID, sent at thoughtTime in their private chat
// with the bot, with fields added or replaced. Its update_id is the message's
// ID.
func update(t *testing.T, field string, fromID int64, fields map[string]any) string {
	t.Helper()
	msg := map[string]any{
		"message_id": messageID,
		"from":       map[string]any{"id": fromID, "is_bot": false, "first_name": "Test"},
		"chat":       map[string]any{"id": fromID, "type": "private"},
		"date":       thoughtTime.Unix(),
	}
	maps.Copy(msg, fields)
	b, err := json.Marshal(map[string]any{"update_id": msg["message_id"], field: msg})
	if err != nil {
		t.Fatalf("encoding update: %v", err)
	}
	return string(b)
}

// withUpdateID is update u with update_id id, as an edit has an update ID of
// its own but the ID of the message it edits.
func withUpdateID(t *testing.T, u string, id int64) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(u), &fields); err != nil {
		t.Fatalf("decoding update: %v", err)
	}
	fields["update_id"] = json.RawMessage(strconv.FormatInt(id, 10))
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encoding update: %v", err)
	}
	return string(b)
}

// sentMessage is what the bot asked the Bot API to send.
type sentMessage struct {
	ChatID    int64
	Text      string
	ReplyToID int64
}

// editedMessage is an edit the bot asked the Bot API to make.
type editedMessage struct {
	ChatID    int64
	MessageID int64
	Text      string
}

// fakeBotAPI stands in for the Telegram Bot API: getUpdates delivers the
// queued updates once, sendChatAction records the action, sendMessage
// records the message and gives it the next message ID, starting at 1001,
// and editMessageText records the edit.
type fakeBotAPI struct {
	mu               sync.Mutex
	updates          []string
	actions          []string
	sent             []sentMessage
	edits            []editedMessage
	isNextSendFailed bool
	isNextEditFailed bool
}

// failNextSend makes the next sendMessage fail, sending nothing.
func (f *fakeBotAPI) failNextSend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isNextSendFailed = true
}

// failNextEdit makes the next editMessageText fail, as Telegram refuses to
// edit a deleted message, editing nothing.
func (f *fakeBotAPI) failNextEdit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isNextEditFailed = true
}

func (f *fakeBotAPI) queue(update string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, update)
}

func (f *fakeBotAPI) chatActions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actions)
}

func (f *fakeBotAPI) sentMessages() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

func (f *fakeBotAPI) editedMessages() []editedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.edits)
}

func (f *fakeBotAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bot"+botToken+"/getUpdates", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		updates := f.updates
		f.updates = nil
		f.mu.Unlock()
		writeResult(t, w, "["+strings.Join(updates, ",")+"]")
	})
	mux.HandleFunc("POST /bot"+botToken+"/sendChatAction", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding sendChatAction: %v", err)
		}
		f.mu.Lock()
		f.actions = append(f.actions, req.Action)
		f.mu.Unlock()
		writeResult(t, w, "true")
	})
	mux.HandleFunc("POST /bot"+botToken+"/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID          int64   `json:"chat_id"`
			Text            string  `json:"text"`
			ParseMode       *string `json:"parse_mode"`
			ReplyParameters struct {
				MessageID int64 `json:"message_id"`
			} `json:"reply_parameters"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding sendMessage: %v", err)
		}
		if req.ParseMode != nil {
			t.Errorf("sendMessage with parse_mode %q, want plain text", *req.ParseMode)
		}
		f.mu.Lock()
		if f.isNextSendFailed {
			f.isNextSendFailed = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
			return
		}
		f.sent = append(f.sent, sentMessage{
			ChatID:    req.ChatID,
			Text:      req.Text,
			ReplyToID: req.ReplyParameters.MessageID,
		})
		id := 1000 + len(f.sent)
		f.mu.Unlock()
		msg, err := json.Marshal(map[string]any{
			"message_id": id,
			"chat":       map[string]any{"id": req.ChatID},
			"date":       0,
			"text":       req.Text,
		})
		if err != nil {
			t.Errorf("encoding sent message: %v", err)
		}
		writeResult(t, w, string(msg))
	})
	mux.HandleFunc("POST /bot"+botToken+"/editMessageText", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID    int64   `json:"chat_id"`
			MessageID int64   `json:"message_id"`
			Text      string  `json:"text"`
			ParseMode *string `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding editMessageText: %v", err)
		}
		if req.ParseMode != nil {
			t.Errorf("editMessageText with parse_mode %q, want plain text", *req.ParseMode)
		}
		f.mu.Lock()
		if f.isNextEditFailed {
			f.isNextEditFailed = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
			return
		}
		f.edits = append(f.edits, editedMessage{ChatID: req.ChatID, MessageID: req.MessageID, Text: req.Text})
		f.mu.Unlock()
		msg, err := json.Marshal(map[string]any{
			"message_id": req.MessageID,
			"chat":       map[string]any{"id": req.ChatID},
			"date":       0,
			"edit_date":  0,
			"text":       req.Text,
		})
		if err != nil {
			t.Errorf("encoding edited message: %v", err)
		}
		writeResult(t, w, string(msg))
	})
	return mux
}

func writeResult(t *testing.T, w http.ResponseWriter, result string) {
	writeJSON(t, w, `{"ok":true,"result":`+result+`}`)
}

func writeJSON(t *testing.T, w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("writing response: %v", err)
	}
}
