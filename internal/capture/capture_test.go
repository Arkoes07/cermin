package capture_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
	messageID = 7 // the Telegram message carrying the Thought
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

func newBot(t *testing.T, classify capture.ClassifyFunc) (*capture.Bot, *sqlite.DB, *fakeBotAPI) {
	t.Helper()
	api := &fakeBotAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)

	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "cermin.db"))
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
	ctx := t.Context()
	if err := bot.Poll(ctx); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if err := bot.ProcessInbox(ctx); err != nil {
		t.Fatalf("ProcessInbox: %v", err)
	}
	thoughts, err := db.Thoughts(ctx)
	if err != nil || len(thoughts) == 0 {
		t.Fatalf("Thoughts = %+v, %v; want some", thoughts, err)
	}
	return thoughts[len(thoughts)-1]
}

// fakeRawOutput is the raw model output of every fakeClassifier
// Classification.
const fakeRawOutput = `{"language":"en","kind":{"value":"idea","confidence":0.9}}`

// fakeClassifier classifies every Thought as an English idea with its
// candidate Topics, and records the existing Topics it was given.
type fakeClassifier struct {
	candidates []cermin.TopicCandidate
	gotTopics  [][]string // the existing Topics given to each call
}

func (f *fakeClassifier) classify(
	_ context.Context,
	_ string,
	_ []cermin.Kind,
	topics []string,
) (cermin.Classification, string, error) {
	f.gotTopics = append(f.gotTopics, topics)
	c := cermin.Classification{
		Kind:             "idea",
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
		fields["reply_to_message"] = map[string]any{
			"message_id": replyToID,
			"chat":       map[string]any{"id": fromID, "type": "private"},
			"date":       thoughtTime.Add(-time.Hour).Unix(),
			"text":       "Buses are always packed at 8am.",
		}
	}
	return update(t, "message", fromID, fields)
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

// sentMessage is what the bot asked the Bot API to send.
type sentMessage struct {
	ChatID    int64
	Text      string
	ReplyToID int64
}

// fakeBotAPI stands in for the Telegram Bot API: getUpdates delivers the
// queued updates once, sendChatAction records the action, and sendMessage
// records the message and gives it the next message ID, starting at 1001.
type fakeBotAPI struct {
	mu               sync.Mutex
	updates          []string
	actions          []string
	sent             []sentMessage
	isNextSendFailed bool
}

// failNextSend makes the next sendMessage fail, sending nothing.
func (f *fakeBotAPI) failNextSend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isNextSendFailed = true
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
