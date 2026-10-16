package chatexport_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/chatexport"
)

func TestParse(t *testing.T) {
	t.Parallel()
	export := `[03/10/26, 19.10.30] Your Name: Maybe the morning walk is when ideas come, not the desk.
[05/10/26, 08.02.11] Your Name: Code review feels slow because nobody owns the queue.
Who would notice if a PR waited a week?
[05/10/26, 12.35.54] Your Name: Kenapa aku selalu nunda nulis sampai malam?
`

	messages, err := chatexport.Parse(strings.NewReader(export))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// The export's times are WIB, 7 hours ahead of UTC.
	want := []chatexport.Message{
		{
			ThoughtAt: time.Date(2026, 10, 3, 12, 10, 30, 0, time.UTC),
			Text:      "Maybe the morning walk is when ideas come, not the desk.",
		},
		{
			ThoughtAt: time.Date(2026, 10, 5, 1, 2, 11, 0, time.UTC),
			Text:      "Code review feels slow because nobody owns the queue.\nWho would notice if a PR waited a week?",
		},
		{
			ThoughtAt: time.Date(2026, 10, 5, 5, 35, 54, 0, time.UTC),
			Text:      "Kenapa aku selalu nunda nulis sampai malam?",
		},
	}
	if !slices.Equal(messages, want) {
		t.Errorf("Parse = %+v, want %+v", messages, want)
	}
}

func TestParse_FailsWhenFirstLineIsNoHeader(t *testing.T) {
	t.Parallel()
	export := "Who would notice if a PR waited a week?\n" +
		"[05/10/26, 12.35.54] Your Name: Kenapa aku selalu nunda nulis sampai malam?\n"

	if messages, err := chatexport.Parse(strings.NewReader(export)); err == nil {
		t.Errorf("Parse = %+v, nil; want an error", messages)
	}
}
