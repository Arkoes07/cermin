package capture_test

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/capture"
	"github.com/arkoes07/cermin/internal/store"
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
	bot, db, api := newBot(t)
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
	want := store.Thought{
		ID:                 got.ID,
		Text:               "What if bus stops showed how full the next bus is?",
		Source:             "telegram",
		TGMessageID:        messageID,
		TGReplyToMessageID: 5,
		AckMessageID:       1001,
	}
	if got != want {
		t.Errorf("Thought = %+v, want %+v", got, want)
	}

	wantSent := []sentMessage{{ChatID: ownerID, Text: "✓ saved", ReplyToID: messageID}}
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
			bot, db, api := newBot(t)
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
			if updates, err := db.UnprocessedUpdates(ctx); err != nil || len(updates) != 0 {
				t.Errorf("unprocessed updates = %+v, %v; want none", updates, err)
			}
		})
	}
}

func TestBot_Process_SafeToRepeat(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	bot, db, api := newBot(t)
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

func newBot(t *testing.T) (*capture.Bot, *store.DB, *fakeBotAPI) {
	t.Helper()
	api := &fakeBotAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)

	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cermin.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})

	return capture.New(db, telegram.New(srv.URL, botToken), ownerID), db, api
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
// with the bot, with fields added or replaced.
func update(t *testing.T, field string, fromID int64, fields map[string]any) string {
	t.Helper()
	msg := map[string]any{
		"message_id": messageID,
		"from":       map[string]any{"id": fromID, "is_bot": false, "first_name": "Test"},
		"chat":       map[string]any{"id": fromID, "type": "private"},
		"date":       thoughtTime.Unix(),
	}
	maps.Copy(msg, fields)
	b, err := json.Marshal(map[string]any{"update_id": 1, field: msg})
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
// queued updates once, and sendMessage records the message and gives it the
// next message ID, starting at 1001.
type fakeBotAPI struct {
	mu      sync.Mutex
	updates []string
	sent    []sentMessage
}

func (f *fakeBotAPI) queue(update string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, update)
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
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write([]byte(`{"ok":true,"result":` + result + `}`)); err != nil {
		t.Errorf("writing response: %v", err)
	}
}
