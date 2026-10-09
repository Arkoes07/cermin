package telegram_test

import (
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
