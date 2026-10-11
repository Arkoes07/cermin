package capture_test

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
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
	messageID = 7    // the Telegram message carrying the Thought
	ackID     = 1001 // its Acknowledgement, the first message the fake Bot API sends
	commandID = 20   // the Telegram message carrying a command
)

// thoughtTime is when the test Thoughts were sent: 08:30 WIB.
var thoughtTime = time.Date(2026, 10, 12, 1, 30, 0, 0, time.UTC)

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

	bot := capture.New(db, telegram.New(srv.URL, botToken), ownerID, classify)
	return bot, db, api
}

// captureQueued polls the queued updates, processes the inbox and returns the
// latest Thought.
func captureQueued(t *testing.T, bot *capture.Bot, db *sqlite.DB) cermin.Thought {
	t.Helper()
	processQueued(t, bot)
	thoughts, err := db.GetThoughts(t.Context())
	if err != nil || len(thoughts) == 0 {
		t.Fatalf("GetThoughts = %+v, %v; want some", thoughts, err)
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

// pollUpdate polls the one queued update and returns it as the inbox holds
// it, unprocessed.
func pollUpdate(t *testing.T, bot *capture.Bot, db *sqlite.DB) sqlite.InboxUpdate {
	t.Helper()
	if err := bot.Poll(t.Context()); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	updates, err := db.GetUnprocessedUpdates(t.Context())
	if err != nil || len(updates) != 1 {
		t.Fatalf("unprocessed updates = %+v, %v; want 1", updates, err)
	}
	return updates[0]
}

// catchUpEvery calls the catch-up n times, once every interval, starting at
// *now, which is the time bot reads, and moving it on by interval after each
// call.
func catchUpEvery(t *testing.T, bot *capture.Bot, now *time.Time, interval time.Duration, n int) {
	t.Helper()
	for range n {
		if err := bot.CatchUp(t.Context()); err != nil {
			t.Fatalf("CatchUp at %v: %v", *now, err)
		}
		*now = now.Add(interval)
	}
}

// classificationsOf returns the Classifications of each of thoughts.
func classificationsOf(t *testing.T, db *sqlite.DB, thoughts []cermin.Thought) [][]cermin.Classification {
	t.Helper()
	all := make([][]cermin.Classification, 0, len(thoughts))
	for _, thought := range thoughts {
		classifications, err := db.GetClassificationsByThoughtID(t.Context(), thought.ID)
		if err != nil {
			t.Fatalf("GetClassificationsByThoughtID: %v", err)
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
) (cermin.Classification, error) {
	f.gotTopics = append(f.gotTopics, topics)
	if f.err != nil {
		return cermin.Classification{}, f.err
	}
	c := cermin.Classification{
		Kind:             cmp.Or(f.kind, "idea"),
		KindConfidence:   0.9,
		Topics:           slices.Clone(f.candidates),
		Model:            "claude-test",
		PromptVersion:    "classify-v1",
		ConfidenceMethod: "verbalized-v1",
		RawOutput:        fakeRawOutput,
		Language:         "en",
	}
	return c, nil
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
// and editMessageText records the edit, or the refusal of an edit of a
// deleted message.
type fakeBotAPI struct {
	mu               sync.Mutex
	updates          []string
	actions          []string
	sent             []sentMessage
	edits            []editedMessage
	refused          []editedMessage // edits of deleted messages
	deleted          []int64         // the IDs of the messages you deleted
	isNextSendFailed bool
	isNextEditFailed bool
}

// failNextSend makes the next sendMessage fail, sending nothing.
func (f *fakeBotAPI) failNextSend() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isNextSendFailed = true
}

// failNextEdit makes the next editMessageText fail, as on a Telegram outage,
// editing nothing.
func (f *fakeBotAPI) failNextEdit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isNextEditFailed = true
}

// deleteMessage deletes message id, as you can in Telegram: every later edit
// of it is refused as Telegram refuses it, and recorded as refused.
func (f *fakeBotAPI) deleteMessage(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
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

func (f *fakeBotAPI) refusedEdits() []editedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.refused)
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
		edit := editedMessage{ChatID: req.ChatID, MessageID: req.MessageID, Text: req.Text}
		f.mu.Lock()
		if f.isNextEditFailed {
			f.isNextEditFailed = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(t, w, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
			return
		}
		if slices.Contains(f.deleted, req.MessageID) {
			f.refused = append(f.refused, edit)
			f.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(t, w, `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`)
			return
		}
		f.edits = append(f.edits, edit)
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
