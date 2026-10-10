package telegram_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/arkoes07/cermin/internal/telegram"
)

func TestClient_SendMessage_ErrorOmitsToken(t *testing.T) {
	t.Parallel()
	const token = "123456:secret-token"
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // so the call fails to connect

	_, err := telegram.New(srv.URL, token).SendMessage(t.Context(), 42, "✓ saved", 7)
	if err == nil {
		t.Fatal("SendMessage to a closed server succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error %q contains the bot token", err)
	}
}

func TestClient_EditMessageText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string // why Telegram refuses the edit
		wantErr     error
	}{
		{
			// As when the same Correction is sent twice: the message already
			// reads the text.
			name: "text unchanged",
			description: "Bad Request: message is not modified: specified new message content and reply markup " +
				"are exactly the same as a current content and reply markup of the message",
		},
		{
			// As when you deleted it.
			name:        "message gone",
			description: "Bad Request: message to edit not found",
			wantErr:     telegram.ErrMessageNotFound,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				body, err := json.Marshal(map[string]any{"ok": false, "error_code": 400, "description": tt.description})
				if err != nil {
					t.Errorf("encoding response: %v", err)
				}
				if _, err := w.Write(body); err != nil {
					t.Errorf("writing response: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			err := telegram.New(srv.URL, "123456:test-token").EditMessageText(
				t.Context(),
				42,
				1001,
				"✏️ 📋 plan · running",
			)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("EditMessageText = %v, want %v", err, tt.wantErr)
			}
		})
	}
}
