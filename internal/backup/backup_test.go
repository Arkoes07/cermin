package backup_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/arkoes07/cermin/internal/backup"
	"github.com/arkoes07/cermin/internal/litestream"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	botToken = "123456:test-token"
	ownerID  = 42
	dbPath   = "/data/cermin.db"
)

// What Litestream answers a sync with: changes waiting, none waiting, or a
// failure.
const (
	waiting  = `{"status":"no_change","path":"/data/cermin.db","txid":4,"replicated_txid":3}`
	caughtUp = `{"status":"no_change","path":"/data/cermin.db","txid":4,"replicated_txid":4}`
	failed   = ""
)

// The messages to the owner.
const (
	behindText   = "⚠️ Backups are behind: changes have waited over an hour to reach R2."
	caughtUpText = "✅ Backups have caught up."
)

// start is the time of each test's first check.
var start = time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)

// check is a check of the backups, at a time after start, when Litestream
// gives answer and Telegram fails every send if isSendFailing.
type check struct {
	at            time.Duration
	answer        string
	isSendFailing bool
}

func TestWatch_Check(t *testing.T) {
	tests := []struct {
		name   string
		checks []check
		want   []string // the messages sent to the owner, in order
	}{
		{
			name:   "waiting under an hour",
			checks: []check{{at: 0, answer: waiting}, {at: 59 * time.Minute, answer: waiting}},
		},
		{
			name:   "waiting an hour",
			checks: []check{{at: 0, answer: waiting}, {at: time.Hour, answer: waiting}},
			want:   []string{behindText},
		},
		{
			name: "still waiting",
			checks: []check{
				{at: 0, answer: waiting},
				{at: time.Hour, answer: waiting},
				{at: 61 * time.Minute, answer: waiting},
				{at: 3 * time.Hour, answer: waiting},
			},
			want: []string{behindText},
		},
		{
			name: "caught up after the warning",
			checks: []check{
				{at: 0, answer: waiting},
				{at: time.Hour, answer: waiting},
				{at: 61 * time.Minute, answer: caughtUp},
				{at: 62 * time.Minute, answer: caughtUp},
			},
			want: []string{behindText, caughtUpText},
		},
		{
			name: "caught up with no earlier warning",
			checks: []check{
				{at: 0, answer: waiting},
				{at: 59 * time.Minute, answer: waiting},
				{at: time.Hour, answer: caughtUp},
			},
		},
		{
			name:   "failed reads",
			checks: []check{{at: 0, answer: failed}, {at: time.Hour, answer: failed}},
			want:   []string{behindText},
		},
		{
			name: "waiting again after catching up",
			checks: []check{
				{at: 0, answer: waiting},
				{at: 30 * time.Minute, answer: caughtUp},
				{at: 31 * time.Minute, answer: waiting},
				{at: 90 * time.Minute, answer: waiting},
			},
		},
		{
			name: "behind again after catching up",
			checks: []check{
				{at: 0, answer: waiting},
				{at: time.Hour, answer: waiting},
				{at: 61 * time.Minute, answer: caughtUp},
				{at: 62 * time.Minute, answer: waiting},
				{at: 122 * time.Minute, answer: waiting},
			},
			want: []string{behindText, caughtUpText, behindText},
		},
		{
			name: "failed sends",
			checks: []check{
				{at: 0, answer: waiting},
				{at: time.Hour, answer: waiting, isSendFailing: true},
				{at: 61 * time.Minute, answer: waiting},
				{at: 62 * time.Minute, answer: caughtUp, isSendFailing: true},
				{at: 63 * time.Minute, answer: caughtUp},
				{at: 64 * time.Minute, answer: caughtUp},
			},
			want: []string{behindText, caughtUpText},
		},
		{
			// The owner's last message says backups are behind, which is true
			// again, so the next stretch sends no repeat.
			name: "waiting again before the caught-up message went",
			checks: []check{
				{at: 0, answer: waiting},
				{at: time.Hour, answer: waiting},
				{at: 61 * time.Minute, answer: caughtUp, isSendFailing: true},
				{at: 62 * time.Minute, answer: waiting},
				{at: 122 * time.Minute, answer: waiting},
				{at: 123 * time.Minute, answer: caughtUp},
			},
			want: []string{behindText, caughtUpText},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, ls, api := newWatch(t)
			for _, c := range tt.checks {
				ls.setAnswer(c.answer)
				api.setSendFailing(c.isSendFailing)
				w.Check(t.Context(), start.Add(c.at))
			}
			if got := api.sentMessages(); !slices.Equal(got, tt.want) {
				t.Errorf("sent %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWatch_Check_UnsyncedSeconds(t *testing.T) {
	w, ls, _ := newWatch(t)
	checks := []struct {
		check
		want float64
	}{
		{check: check{at: 0, answer: caughtUp}, want: 0},
		{check: check{at: time.Minute, answer: waiting}, want: 0},
		{check: check{at: 2*time.Minute + 30*time.Second, answer: waiting}, want: 90},
		{check: check{at: 3 * time.Minute, answer: failed}, want: 120},
		{check: check{at: 4 * time.Minute, answer: caughtUp}, want: 0},
	}
	for _, c := range checks {
		ls.setAnswer(c.answer)
		w.Check(t.Context(), start.Add(c.at))
		if got := testutil.ToFloat64(backup.UnsyncedSeconds); got != c.want {
			t.Errorf("after the check at %v, cermin_backup_unsynced_seconds = %v, want %v", c.at, got, c.want)
		}
	}
}

// newWatch returns a Watch that asks a fake Litestream and sends through a
// fake Bot API.
func newWatch(t *testing.T) (*backup.Watch, *fakeLitestream, *fakeBotAPI) {
	t.Helper()
	ls := &fakeLitestream{}
	api := &fakeBotAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	w := backup.New(litestream.New(serveUnix(t, ls.handler(t)), dbPath), telegram.New(srv.URL, botToken), ownerID)
	return w, ls, api
}

// fakeLitestream answers every sync with its answer, or with a failure if
// that is failed.
type fakeLitestream struct {
	mu     sync.Mutex
	answer string
}

func (f *fakeLitestream) setAnswer(answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

func (f *fakeLitestream) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		answer := f.answer
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if answer == failed {
			w.WriteHeader(http.StatusInternalServerError)
			answer = `{"error":"cannot determine current position"}`
		}
		writeBody(t, w, answer)
	})
	return mux
}

// fakeBotAPI stands in for the Telegram Bot API: sendMessage records the text
// it sends the owner, or fails while isSendFailing.
type fakeBotAPI struct {
	mu            sync.Mutex
	sent          []string
	isSendFailing bool
}

func (f *fakeBotAPI) setSendFailing(isFailing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isSendFailing = isFailing
}

func (f *fakeBotAPI) sentMessages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

func (f *fakeBotAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /bot"+botToken+"/sendMessage", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ChatID    int64   `json:"chat_id"`
			Text      string  `json:"text"`
			ParseMode *string `json:"parse_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding sendMessage: %v", err)
		}
		if req.ChatID != ownerID || req.ParseMode != nil {
			t.Errorf(
				"sendMessage to chat %d with parse_mode %v, want to %d as plain text",
				req.ChatID,
				req.ParseMode,
				ownerID,
			)
		}
		w.Header().Set("Content-Type", "application/json")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.isSendFailing {
			w.WriteHeader(http.StatusInternalServerError)
			writeBody(t, w, `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
			return
		}
		f.sent = append(f.sent, req.Text)
		writeBody(t, w, `{"ok":true,"result":{"message_id":1001,"chat":{"id":42},"date":0}}`)
	})
	return mux
}

func writeBody(t *testing.T, w http.ResponseWriter, body string) {
	if _, err := w.Write([]byte(body)); err != nil {
		t.Errorf("writing response: %v", err)
	}
}

// serveUnix serves h on a unix socket in a new temp dir and returns the
// socket's path.
func serveUnix(t *testing.T, h http.Handler) string {
	t.Helper()
	// Not t.TempDir, whose path carries the test's name: a socket's path must
	// stay under 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "litestream")
	if err != nil {
		t.Fatalf("making temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing temp dir: %v", err)
		}
	})
	ln, err := net.Listen("unix", filepath.Join(dir, "ls.sock"))
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return ln.Addr().String()
}
