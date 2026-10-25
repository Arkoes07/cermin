package main

import (
	"bytes"
	"encoding/csv"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// embeddedThought is a Thought with text, sent at 10:00 WIB on the October
// day day, and vector.
func embeddedThought(id int64, day int, text string, vector ...float32) cermin.EmbeddedThought {
	return cermin.EmbeddedThought{
		Thought: cermin.Thought{ID: id, Text: text, ThoughtAt: time.Date(2026, 10, day, 3, 0, 0, 0, time.UTC)},
		Vector:  vector,
	}
}

// The Thoughts are 2 days apart, so none is remembered. Thought 4 scores 0.8
// against Thoughts 1 and 2 and 0.6 against Thought 3, so it shortlists all
// three; Thought 3 scores 0 against the others, so it shortlists none.
func TestWriteShortlists(t *testing.T) {
	t.Parallel()
	corpus := []cermin.EmbeddedThought{
		embeddedThought(1, 1, "Run 5 km every Saturday.", 1, 0),
		embeddedThought(2, 3, "Lari 5 km tiap Sabtu, mulai minggu ini.", 1, 0),
		embeddedThought(3, 5, "Why is the cafe always full at 3 pm?", 0, 1),
		embeddedThought(4, 7, "Ran 5 km today, legs are sore.", 4, 3),
	}
	// In the format of #27's CSV, before rank and held_out.
	old := `score,thought_id,past_id,thought_date,past_date,thought_text,past_text,wanted
1.0000,2,1,2026-10-03,2026-10-01,a,b,y
0.8000,4,2,2026-10-07,2026-10-03,c,d,n
0.0000,3,1,2026-10-05,2026-10-01,e,f,n
`
	labels, err := readLabels(strings.NewReader(old))
	if err != nil {
		t.Fatalf("readLabels: %v", err)
	}

	var out bytes.Buffer
	written, carried, err := writeShortlists(&out, corpus, labels)
	if err != nil {
		t.Fatalf("writeShortlists: %v", err)
	}

	got, err := csv.NewReader(&out).ReadAll()
	if err != nil {
		t.Fatalf("reading CSV: %v", err)
	}
	header := []string{
		"score", "rank", "thought_id", "past_id", "thought_date", "past_date", "thought_text", "past_text", "held_out",
		"wanted",
	}
	// The pairs of Thoughts 2 and 1 and of Thoughts 4 and 2 were labelled
	// before, so they keep their labels, with held_out n.
	want := [][]string{
		header,
		{"1.0000", "1", "2", "1", "2026-10-03", "2026-10-01", corpus[1].Text, corpus[0].Text, "n", "y"},
		{"0.8000", "1", "4", "1", "2026-10-07", "2026-10-01", corpus[3].Text, corpus[0].Text, "y", ""},
		{"0.8000", "2", "4", "2", "2026-10-07", "2026-10-03", corpus[3].Text, corpus[1].Text, "n", "n"},
		{"0.6000", "3", "4", "3", "2026-10-07", "2026-10-05", corpus[3].Text, corpus[2].Text, "y", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("writeShortlists wrote\n%q\nwant\n%q", got, want)
	}
	// The label of Thought 3's pair, below the floor, is dropped.
	if written != 4 || carried != 2 {
		t.Errorf("writeShortlists = %d written, %d carried; want 4, 2", written, carried)
	}
}
