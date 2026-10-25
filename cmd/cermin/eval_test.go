package main

import (
	"context"
	"encoding/csv"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
)

// openDB opens a new database in a temporary directory.
func openDB(t *testing.T) *sqlite.DB {
	t.Helper()
	db, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "cermin.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// cerminClassification is Cermin's Classification of the Thought thoughtID, as
// kind.
func cerminClassification(thoughtID int64, kind string) cermin.Classification {
	return cermin.Classification{
		ThoughtID:        thoughtID,
		Source:           cermin.SourceCermin,
		Kind:             kind,
		KindConfidence:   0.9,
		Model:            "claude-test",
		PromptVersion:    "classify-v1",
		ConfidenceMethod: "verbalized-v1",
		RawOutput:        "{}",
		Language:         "en",
	}
}

// Thought 6's Correction replaces the Kind it shared with Thought 2, so their
// pair doesn't share one. Thought 7 has no Classification. The CSV lists
// Thought 4's pairs out of rank order.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	texts := []string{
		"", // IDs start at 1
		"Run 5 km every Saturday morning.",
		"Kenapa masakan rumah selalu lebih enak dari restoran?",
		"Idea: a running route app that avoids hills on trips.",
		"Ran 5 km this Saturday and kept the plan.",
		"Should I pack running shoes for the Bali trip?",
		"Masak sendiri ternyata lebih hemat.",
		"Bought a new phone case.",
	}
	for i, text := range texts[1:] {
		thought, err := db.SaveThought(ctx, cermin.Thought{
			Text:        text,
			Source:      "telegram",
			TGMessageID: int64(100 + i),
			ThoughtAt:   time.Date(2026, 10, 2*i+1, 3, 0, 0, 0, time.UTC),
			ReceivedAt:  time.Now(),
		})
		if err != nil || thought.ID != int64(i+1) {
			t.Fatalf("SaveThought = %+v, %v; want ID %d", thought, err, i+1)
		}
	}
	classifications := []cermin.Classification{
		cerminClassification(1, "plan"),
		cerminClassification(2, "observation"),
		cerminClassification(3, "idea"),
		cerminClassification(4, "plan"),
		cerminClassification(5, "idea"),
		cerminClassification(6, "observation"),
		{ThoughtID: 6, Source: cermin.SourceCorrection, Kind: "question", TGMessageID: 200},
	}
	for _, c := range classifications {
		if _, err := db.SaveClassification(ctx, c); err != nil {
			t.Fatalf("SaveClassification: %v", err)
		}
	}

	// confidences is the fake Judge's confidence for each pair, by its new
	// and its past Thought.
	confidences := map[[2]int]float64{
		{4, 1}: 0.9, {4, 2}: 0.3, {5, 3}: 0.8, {5, 1}: 0.65, {6, 2}: 0.55, {6, 3}: 0.2, {7, 1}: 0.1,
	}
	rows := []struct {
		score, rank     string
		thought, past   int
		heldOut, wanted string
	}{
		{score: "0.6000", rank: "2", thought: 4, past: 2, heldOut: "n", wanted: "n"},
		{score: "0.7000", rank: "1", thought: 4, past: 1, heldOut: "n", wanted: "y"},
		{score: "0.6500", rank: "1", thought: 5, past: 3, heldOut: "y", wanted: "y"},
		{score: "0.5000", rank: "2", thought: 5, past: 1, heldOut: "y", wanted: "n"},
		{score: "0.5500", rank: "1", thought: 6, past: 2, heldOut: "y", wanted: "Y"},
		{score: "0.4800", rank: "2", thought: 6, past: 3, heldOut: "y", wanted: "n"},
		{score: "0.4600", rank: "1", thought: 7, past: 1, heldOut: "y", wanted: " n"},
	}
	var labelled strings.Builder
	w := csv.NewWriter(&labelled)
	records := [][]string{{"score", "rank", "thought_id", "past_id", "thought_text", "past_text", "held_out", "wanted"}}
	for _, r := range rows {
		records = append(records, []string{
			r.score,
			r.rank,
			strconv.Itoa(r.thought),
			strconv.Itoa(r.past),
			texts[r.thought],
			texts[r.past],
			r.heldOut,
			r.wanted,
		})
	}
	if err := w.WriteAll(records); err != nil {
		t.Fatalf("writing CSV: %v", err)
	}

	judged := [][]string{}
	judge := func(_ context.Context, text string, shortlist []string) ([]float64, error) {
		judged = append(judged, append([]string{text}, shortlist...))
		out := []float64{}
		for _, past := range shortlist {
			out = append(out, confidences[[2]int{slices.Index(texts, text), slices.Index(texts, past)}])
		}
		return out, nil
	}
	pairs, err := preparePairs(ctx, strings.NewReader(labelled.String()), db, judge)
	if err != nil {
		t.Fatalf("preparePairs: %v", err)
	}
	got := evaluate(pairs)

	// One call per Thought, for its shortlist in rank order.
	wantJudged := [][]string{
		{texts[4], texts[1], texts[2]},
		{texts[5], texts[3], texts[1]},
		{texts[6], texts[2], texts[3]},
		{texts[7], texts[1]},
	}
	if !reflect.DeepEqual(judged, wantJudged) {
		t.Errorf("judged %q, want %q", judged, wantJudged)
	}
	// Each tally is positional, to keep the table readable: pairs kept, wanted
	// pairs kept, precision, recall, and Thoughts that would get a
	// Resurfacing.
	want := evaluation{
		lines: []line{
			{method: "C at 0.6500", rows: "all", tally: tally{2, 2, 1, 2.0 / 3, 2}},
			{method: "C at 0.5500", rows: "held-out", tally: tally{2, 2, 1, 1, 2}},
			{method: "J at 0.5", rows: "all", tally: tally{4, 3, 0.75, 1, 3}},
			{method: "J at 0.5", rows: "held-out", tally: tally{3, 2, 2.0 / 3, 1, 2}},
			{method: "J at 0.6", rows: "all", tally: tally{3, 2, 2.0 / 3, 2.0 / 3, 2}},
			{method: "J at 0.6", rows: "held-out", tally: tally{2, 1, 0.5, 0.5, 1}},
			{method: "J at 0.7", rows: "all", tally: tally{2, 2, 1, 2.0 / 3, 2}},
			{method: "J at 0.7", rows: "held-out", tally: tally{1, 1, 1, 0.5, 1}},
			{method: "J at 0.8", rows: "all", tally: tally{2, 2, 1, 2.0 / 3, 2}},
			{method: "J at 0.8", rows: "held-out", tally: tally{1, 1, 1, 0.5, 1}},
		},
		kindsAll:     kindOverlap{wantedShared: 2, wanted: 3, unwantedShared: 0, unwanted: 4},
		kindsHeldOut: kindOverlap{wantedShared: 1, wanted: 2, unwantedShared: 0, unwanted: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("evaluate =\n%+v\nwant\n%+v", got, want)
	}
}

// C reads at the lowest cut-off that passes the bar, even where a higher one
// is more precise. If none passes, it reads at the most precise one, the
// lowest on a tie.
func TestEvaluate_Cosine(t *testing.T) {
	t.Parallel()
	scoredPair := func(score float64, isWanted bool) labelledPair {
		return labelledPair{score: score, isWanted: isWanted}
	}
	tests := []struct {
		name  string
		pairs []labelledPair
		want  string
	}{
		{
			name: "lowest that passes",
			pairs: []labelledPair{
				scoredPair(0.9, true),
				scoredPair(0.8, true),
				scoredPair(0.7, true),
				scoredPair(0.6, true),
				scoredPair(0.5, false),
				scoredPair(0.4, false),
			},
			want: "C at 0.5000",
		},
		{
			name: "none passes",
			pairs: []labelledPair{
				scoredPair(0.9, false),
				scoredPair(0.8, true),
				scoredPair(0.7, false),
				scoredPair(0.6, true),
			},
			want: "C at 0.6000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := evaluate(tt.pairs).lines[0].method; got != tt.want {
				t.Errorf("C = %q, want %q", got, tt.want)
			}
		})
	}
}
