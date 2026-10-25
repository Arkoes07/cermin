package claude_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/claude"
)

func TestClassifier_Classify_RecordsCall(t *testing.T) {
	// Not parallel: the metrics are global, and t.Setenv points the client at
	// the fake API.
	const (
		model = "claude-test"
		text  = "What if bus stops showed how full the next bus is?"
	)
	tests := []struct {
		name        string
		status      int
		output      string // the model's text, if status is 200
		wantOutcome string
		wantTokens  bool
	}{
		{
			name:   "ok",
			status: http.StatusOK,
			output: `{"language":"en","kind":{"value":"idea","confidence":0.9},` +
				`"topics":[{"name":"transport","confidence":0.8}],"kind_suggestion":""}`,
			wantOutcome: "ok",
			wantTokens:  true,
		},
		{name: "api error", status: http.StatusBadRequest, wantOutcome: "error"},
		// Billed, so its tokens count.
		{
			name:        "output fails to parse",
			status:      http.StatusOK,
			output:      "not json",
			wantOutcome: "error",
			wantTokens:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(fakeMessagesAPI(t, tt.status, "end_turn", tt.output))
			t.Cleanup(srv.Close)
			t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
			kinds := []cermin.Kind{{Name: "idea", Definition: "something I might do or make"}}

			before := callMetricsOf(t, model, tt.wantOutcome)
			_, err := claude.New("test-key", model).Classify(t.Context(), text, kinds, nil)
			if isErr := err != nil; isErr != (tt.wantOutcome == "error") {
				t.Fatalf("Classify: %v, want outcome %s", err, tt.wantOutcome)
			}
			after := callMetricsOf(t, model, tt.wantOutcome)

			got := callMetrics{
				calls:        after.calls - before.calls,
				durations:    after.durations - before.durations,
				inputTokens:  after.inputTokens - before.inputTokens,
				outputTokens: after.outputTokens - before.outputTokens,
			}
			want := callMetrics{calls: 1, durations: 1}
			if tt.wantTokens {
				want.inputTokens, want.outputTokens = 120, 30
			}
			if got != want {
				t.Errorf("recorded %+v, want %+v", got, want)
			}
		})
	}
}

// callMetrics is what the metrics hold for classify calls to one model that
// ended with one outcome.
type callMetrics struct {
	calls, durations, inputTokens, outputTokens float64
}

func callMetricsOf(t *testing.T, model, outcome string) callMetrics {
	t.Helper()
	durations := claude.RequestDuration.WithLabelValues("classify", model)
	var duration dto.Metric
	if err := durations.(prometheus.Metric).Write(&duration); err != nil {
		t.Fatalf("reading duration: %v", err)
	}
	return callMetrics{
		calls:        testutil.ToFloat64(claude.RequestsTotal.WithLabelValues("classify", model, outcome)),
		durations:    float64(duration.GetHistogram().GetSampleCount()),
		inputTokens:  testutil.ToFloat64(claude.TokensTotal.WithLabelValues("classify", model, "input")),
		outputTokens: testutil.ToFloat64(claude.TokensTotal.WithLabelValues("classify", model, "output")),
	}
}

// fakeMessagesAPI answers every request with status: an error, or, for 200, a
// message that stopped with stopReason, whose text is output and whose usage
// is 120 input and 30 output tokens.
func fakeMessagesAPI(t *testing.T, status int, stopReason, output string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "invalid_request_error", "message": "made-up failure"},
		}
		if status == http.StatusOK {
			body = map[string]any{
				"id":          "msg_test",
				"type":        "message",
				"role":        "assistant",
				"model":       "claude-test",
				"stop_reason": stopReason,
				"content":     []map[string]any{{"type": "text", "text": output}},
				"usage":       map[string]any{"input_tokens": 120, "output_tokens": 30},
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("writing response: %v", err)
		}
	})
}
