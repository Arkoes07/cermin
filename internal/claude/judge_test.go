package claude_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/arkoes07/cermin/internal/claude"
)

func TestJudge_JudgeShortlist(t *testing.T) {
	// Not parallel: the metrics are global, and t.Setenv points the client at
	// the fake API.
	const (
		model = "claude-test"
		text  = "My back hurts after long days at the desk."
	)
	shortlist := []string{"Should I buy a standing desk?", "Kursi kantor baru bikin punggung lebih enak."}
	tests := []struct {
		name       string
		stopReason string
		output     string
		want       []float64 // nil for an error
	}{
		{
			name:       "one confidence per shortlisted Thought",
			stopReason: "end_turn",
			output:     `{"confidences":[0.9,0.2]}`,
			want:       []float64{0.9, 0.2},
		},
		{name: "too few confidences", stopReason: "end_turn", output: `{"confidences":[0.9]}`},
		{name: "too many confidences", stopReason: "end_turn", output: `{"confidences":[0.9,0.2,0.1]}`},
		{name: "cut off", stopReason: "max_tokens", output: `{"confidences":[0.9,0.2]}`},
		{name: "refused", stopReason: "refusal", output: `{"confidences":[0.9,0.2]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(fakeMessagesAPI(t, http.StatusOK, tt.stopReason, tt.output))
			t.Cleanup(srv.Close)
			t.Setenv("ANTHROPIC_BASE_URL", srv.URL)
			outcome := "ok"
			if tt.want == nil {
				outcome = "error"
			}

			calls := claude.RequestsTotal.WithLabelValues("judge", model, outcome)
			before := testutil.ToFloat64(calls)
			got, err := claude.NewJudge("test-key", model).JudgeShortlist(t.Context(), text, shortlist)
			if isErr := err != nil; isErr != (tt.want == nil) || !slices.Equal(got, tt.want) {
				t.Errorf("JudgeShortlist = %v, %v; want %v", got, err, tt.want)
			}
			if recorded := testutil.ToFloat64(calls) - before; recorded != 1 {
				t.Errorf("recorded %v judge calls with outcome %s, want 1", recorded, outcome)
			}
		})
	}
}
