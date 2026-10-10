//go:build integration

package claude_test

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/sqlite"
)

// TestClassifier_Classify calls Claude, so it needs ANTHROPIC_API_KEY:
//
//	go test -tags integration -v ./internal/claude/
func TestClassifier_Classify(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY is not set")
	}
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "cermin.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing store: %v", err)
		}
	})
	kinds := db.Kinds()
	classifier := claude.New(apiKey, cmp.Or(os.Getenv("CERMIN_CLASSIFY_MODEL"), claude.DefaultModel))

	tests := []struct {
		name         string
		text         string
		wantLanguage string
	}{
		{name: "english", text: "What if bus stops showed how full the next bus is?", wantLanguage: "en"},
		{name: "bahasa indonesia", text: "Kenapa kafe dekat kantor selalu penuh jam 3 sore?", wantLanguage: "id"},
		{name: "mixed", text: "Besok mulai lari pagi tiap Saturday, biar lebih fit before the trip.", wantLanguage: "mixed"},
	}
	// Not parallel, so the logged times show the first call's extra latency.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			c, err := classifier.Classify(
				t.Context(),
				tt.text,
				kinds,
				[]string{"business", "transport"},
			)
			if err != nil {
				t.Fatalf("Classify: %v", err)
			}
			t.Logf("%s in %v: %s", c.Model, time.Since(start).Round(time.Millisecond), c.RawOutput)

			if c.Language != tt.wantLanguage {
				t.Errorf("Language = %q, want %q", c.Language, tt.wantLanguage)
			}
			if !slices.ContainsFunc(kinds, func(k cermin.Kind) bool { return k.Name == c.Kind }) {
				t.Errorf("Kind = %q, want one of the Kinds", c.Kind)
			}
			if len(c.Topics) == 0 {
				t.Error("no candidate Topics")
			}
			isRecorded := c.Model != "" && c.PromptVersion != "" && c.ConfidenceMethod != "" && c.RawOutput != ""
			if !isRecorded {
				t.Errorf("Classification %+v lacks model, prompt version, confidence method or raw output", c)
			}
		})
	}
}
