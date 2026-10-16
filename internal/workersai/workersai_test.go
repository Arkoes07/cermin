package workersai_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/arkoes07/cermin/internal/workersai"
)

const (
	accountID = "test-account"
	apiToken  = "test-workersai-token"
	model     = "@cf/test/embedding"
)

// More texts than one call takes go out in calls of 32, and their vectors
// come back in the order of the texts.
func TestClient_Embed(t *testing.T) {
	t.Parallel()
	api := &fakeEmbedAPI{}
	srv := httptest.NewServer(api.handler(t))
	t.Cleanup(srv.Close)
	texts := make([]string, 33)
	for i := range texts {
		texts[i] = fmt.Sprintf("Made-up Thought %d about bus stops.", i)
	}

	vectors, err := workersai.New(srv.URL, accountID, apiToken, model).Embed(t.Context(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}

	calls := api.calls()
	sizes := []int{}
	for _, c := range calls {
		sizes = append(sizes, len(c))
	}
	if !slices.Equal(sizes, []int{32, 1}) {
		t.Errorf("calls took %v texts, want [32 1]", sizes)
	}
	if sent := slices.Concat(calls...); !slices.Equal(sent, texts) {
		t.Errorf("sent texts %q, want %q", sent, texts)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("got %d vectors, want %d", len(vectors), len(texts))
	}
	for i, v := range vectors {
		if want := vectorOf(texts[i]); !slices.Equal(v, want) {
			t.Errorf("vector %d = %v, want %v", i, v, want)
		}
	}
}

// The token travels in a header, so an error never carries it, even one that
// quotes the request's URL.
func TestClient_Embed_ErrorLeavesOutToken(t *testing.T) {
	t.Parallel()
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		body := `{"result":null,"success":false,"errors":[{"code":5006,"message":"made-up failure"}],"messages":[]}`
		if _, err := fmt.Fprint(w, body); err != nil {
			t.Errorf("writing response: %v", err)
		}
	}))
	t.Cleanup(refusing.Close)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()

	tests := []struct {
		name    string
		apiURL  string
		wantErr string // part of the error
	}{
		{name: "api error", apiURL: refusing.URL, wantErr: "status 400: made-up failure (code 5006)"},
		{name: "unreachable", apiURL: closed.URL, wantErr: "refused"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			client := workersai.New(tt.apiURL, accountID, apiToken, model)
			_, err := client.Embed(t.Context(), []string{"A made-up Thought."})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Embed: %v, want an error containing %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), apiToken) {
				t.Errorf("Embed: %v, which contains the token", err)
			}
		})
	}
}

// vectorOf is the vector fakeEmbedAPI gives text: its length, and its first
// byte.
func vectorOf(text string) []float32 {
	return []float32{float32(len(text)), float32(text[0])}
}

// fakeEmbedAPI stands in for Workers AI running model for accountID, with the
// token apiToken. It checks each request, records its texts, and answers with
// the vector of each text, from vectorOf.
type fakeEmbedAPI struct {
	mu    sync.Mutex
	texts [][]string // the texts of each call
}

func (f *fakeEmbedAPI) calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.texts)
}

func (f *fakeEmbedAPI) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /accounts/"+accountID+"/ai/run/"+model, func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer "+apiToken {
			t.Errorf("Authorization = %q, want %q", auth, "Bearer "+apiToken)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want none", r.URL.RawQuery)
		}
		// Unknown fields fail it, so queries or an instruction would.
		var req struct {
			Text []string `json:"text"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			t.Errorf("decoding request: %v", err)
		}

		data := [][]float32{}
		for _, text := range req.Text {
			data = append(data, vectorOf(text))
		}
		f.mu.Lock()
		f.texts = append(f.texts, req.Text)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"result":   map[string]any{"data": data, "shape": []int{len(data), 2}},
			"success":  true,
			"errors":   []any{},
			"messages": []any{},
		}
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			t.Errorf("writing response: %v", err)
		}
	})
	return mux
}
