//go:build integration

package claude_test

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/sqlite"
)

// TestClassifier_Classify calls Claude, so it needs ANTHROPIC_API_KEY:
//
//	go test -tags integration -v ./internal/claude/
func TestClassifier_Classify(t *testing.T) {
	classifier, kinds := newClassifier(t)

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
			c, err := classifier.Classify(t.Context(), tt.text, kinds, []string{"business", "transport"})
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

// TestClassifier_TopicReuse classifies made-up Thoughts in order, each given
// the Topics the ones before it kept, as the bot does, and logs how broad and
// reused the kept Topics are. Model output varies from run to run, so it fails
// only if a kept Topic names a Kind:
//
//	go test -tags integration -v -run TopicReuse ./internal/claude/
func TestClassifier_TopicReuse(t *testing.T) {
	classifier, kinds := newClassifier(t)

	// Clusters of three share a broad area in different words. The traps
	// invite a Topic that names the Kind, or a narrow one. Shuffled, so no
	// cluster is sent back to back.
	thoughts := []struct {
		group string
		text  string
	}{
		{group: "money", text: "Bought new noise-cancelling headphones today, probably should have waited for the sale."},
		{group: "health", text: "Tidur cuma 5 jam tiga malam berturut-turut, badan rasanya berat."},
		{group: "kind trap", text: "Business idea: a subscription that delivers fresh beans from local coffee roasters."},
		{group: "coding", text: "Spent two hours on a bug that turned out to be a timezone mismatch in the tests."},
		{group: "family", text: "Ibu minta dijenguk weekend ini, and I keep postponing it."},
		{group: "narrow trap", text: "Mobil sudah tiga kali masuk bengkel tahun ini, mungkin sebaiknya dijual saja."},
		{group: "money", text: "Gaji bulan ini masuk telat, harus atur ulang jadwal bayar tagihan."},
		{group: "kind trap", text: "Pertanyaan soal coding: kenapa banyak tim masih memakai monorepo?"},
		{group: "health", text: "My knee hurts after every run longer than 5k."},
		{group: "coding", text: "Pengen belajar Rust, tapi not sure kapan sempat with all these deadlines."},
		{group: "family", text: "My sister and I haven't really talked since the wedding."},
		{group: "kind trap", text: "Plan buat liburan akhir tahun: Jogja tiga hari, then Bali."},
		{group: "money", text: "Target nabung 20 juta sebelum akhir tahun, but honestly not sure it's realistic."},
		{group: "health", text: "Mulai kurangi gula minggu ini, let's see if my energy improves."},
		{group: "narrow trap", text: "Mau cancel tiga streaming subscriptions yang jarang dipakai."},
		{group: "coding", text: "Membaca kode orang lain ternyata lebih melelahkan daripada menulis sendiri."},
		{group: "kind trap", text: "Random observation: nobody at the office answers messages after 6pm."},
		{group: "family", text: "Anak-anak makin jarang makan malam bersama di rumah."},
	}

	var table strings.Builder
	table.WriteString("| # | Group | Thought | Kind | Kept | Dropped |\n|---|---|---|---|---|---|\n")
	timesKept := map[string]int{} // by Topic name
	for i, thought := range thoughts {
		c, err := classifier.Classify(t.Context(), thought.text, kinds, slices.Sorted(maps.Keys(timesKept)))
		if err != nil {
			t.Fatalf("Classify thought %d: %v", i+1, err)
		}
		kept, dropped := []string{}, []string{}
		for _, topic := range cermin.KeepTopics(c.Topics, nil) {
			name := fmt.Sprintf("%s %.2f", topic.Name, topic.Confidence)
			if !topic.Kept {
				dropped = append(dropped, name)
				continue
			}
			kept = append(kept, name)
			timesKept[topic.Name]++
		}
		fmt.Fprintf(
			&table,
			"| %d | %s | %s | %s | %s | %s |\n",
			i+1,
			thought.group,
			thought.text,
			c.Kind,
			strings.Join(kept, ", "),
			strings.Join(dropped, ", "),
		)
	}

	var reused, leaks int
	for name, n := range timesKept {
		if n >= 2 {
			reused++
		}
		if namesKind(name, kinds) {
			leaks++
			t.Errorf("kept Topic %q names a Kind", name)
		}
	}
	t.Logf(
		"%s\nTopics created: %d. Kept by 2 or more Thoughts: %d. Naming a Kind: %d.",
		table.String(),
		len(timesKept),
		reused,
		leaks,
	)
}

// newClassifier returns a Classifier for ANTHROPIC_API_KEY, on
// CERMIN_CLASSIFY_MODEL or the default model, and the starting Kinds. It skips
// the test without a key.
func newClassifier(t *testing.T) (*claude.Classifier, []cermin.Kind) {
	t.Helper()
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
	classifier := claude.New(apiKey, cmp.Or(os.Getenv("CERMIN_CLASSIFY_MODEL"), claude.DefaultModel))
	return classifier, db.Kinds()
}

// namesKind reports whether topic contains the name of one of kinds as a whole
// word.
func namesKind(topic string, kinds []cermin.Kind) bool {
	words := strings.FieldsFunc(topic, func(r rune) bool { return !unicode.IsLetter(r) })
	return slices.ContainsFunc(kinds, func(k cermin.Kind) bool { return slices.Contains(words, k.Name) })
}
