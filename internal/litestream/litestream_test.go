package litestream_test

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/arkoes07/cermin/internal/litestream"
)

const dbPath = "/data/cermin.db"

func TestClient_Sync(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		status  int
		body    string
		want    litestream.Position
		wantErr string // "" if none
	}{
		{
			name:   "positions",
			status: http.StatusOK,
			body:   `{"status":"no_change","path":"/data/cermin.db","txid":4,"replicated_txid":3}`,
			want:   litestream.Position{TXID: 4, ReplicatedTXID: 3},
		},
		{
			name:    "refusal",
			status:  http.StatusNotFound,
			body:    `{"error":"database not found: /data/cermin.db"}`,
			wantErr: `litestream: sync: status 404: {"error":"database not found: /data/cermin.db"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			socket := serveUnix(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Path string `json:"path"`
					Wait bool   `json:"wait"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decoding request: %v", err)
				}
				if r.Method != http.MethodPost || r.URL.Path != "/sync" || req.Path != dbPath || req.Wait {
					t.Errorf(
						"request = %s %s %+v, want POST /sync for %s without wait",
						r.Method,
						r.URL.Path,
						req,
						dbPath,
					)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if _, err := w.Write([]byte(tt.body)); err != nil {
					t.Errorf("writing response: %v", err)
				}
			}))

			got, err := litestream.New(socket, dbPath).Sync(t.Context())
			gotErr := ""
			if err != nil {
				gotErr = err.Error()
			}
			if got != tt.want || gotErr != tt.wantErr {
				t.Errorf("Sync = %+v, %q; want %+v, %q", got, gotErr, tt.want, tt.wantErr)
			}
		})
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
