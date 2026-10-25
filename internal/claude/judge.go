package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// DefaultJudgeModel is the Claude model that judges shortlists unless
// configured otherwise: in #29 it kept more wanted pairs than Haiku 5.5, at a
// higher precision.
const DefaultJudgeModel = "claude-sonnet-5-5"

const (
	// JudgePromptVersion names the prompt in judgeSystemPrompt. Change it with
	// the prompt, so that Resurfacings can be compared per version.
	JudgePromptVersion = "judge-v1"
	// KeepConfidence is the confidence at which a shortlisted Thought is kept
	// as strongly related: the lowest at which, in #29, at least 8 in 10 of the
	// pairs DefaultJudgeModel kept were wanted.
	KeepConfidence = 0.6
	// purposeJudge labels the metrics of judge calls.
	purposeJudge = "judge"
)

// Judge judges, with a Claude model, which past Thoughts shortlisted for a
// Resurfacing are strongly related to the new one.
type Judge struct {
	client anthropic.Client
	model  string
}

// NewJudge returns a Judge that calls model with the Anthropic API key apiKey.
func NewJudge(apiKey, model string) *Judge {
	initCallMetrics(purposeJudge, model)
	return &Judge{client: anthropic.NewClient(option.WithAPIKey(apiKey)), model: model}
}

// JudgeShortlist asks Claude how likely each past Thought in shortlist, given
// by its text, is to be strongly related to the new Thought's text. It returns
// one confidence from 0 to 1 per shortlisted Thought, in order: Claude's own
// estimate (verbalized-v1). It returns no reasons, since a reason would
// paraphrase a Thought and outlive its deletion.
func (j *Judge) JudgeShortlist(ctx context.Context, text string, shortlist []string) (_ []float64, err error) {
	input := renderJudgeInput(text, shortlist)
	start := time.Now()
	resp, err := j.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     j.model,
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: judgeSystemPrompt}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(input))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: judgeOutputSchema},
		},
	})
	elapsed := time.Since(start)
	// Recorded as JudgeShortlist returns, so that output that fails to parse
	// counts as an error.
	defer func() { recordCall(purposeJudge, j.model, elapsed, resp, err) }()
	if err != nil {
		return nil, fmt.Errorf("judge: %w", err)
	}
	// A refused or cut-off output may not match the schema.
	if resp.StopReason != anthropic.StopReasonEndTurn {
		return nil, fmt.Errorf("judge: model stopped with %s", resp.StopReason)
	}

	var out struct {
		Confidences []float64 `json:"confidences"`
	}
	if err := json.Unmarshal([]byte(readOutput(resp)), &out); err != nil {
		return nil, fmt.Errorf("judge: decoding output: %w", err)
	}
	// The schema can't fix the array's length.
	if len(out.Confidences) != len(shortlist) {
		return nil, fmt.Errorf(
			"judge: %d confidences for %d shortlisted thoughts",
			len(out.Confidences),
			len(shortlist),
		)
	}
	return out.Confidences, nil
}

// judgeOutputSchema is the JSON schema the Judge's output must follow.
var judgeOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"confidences": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
	},
	"required":             []string{"confidences"},
	"additionalProperties": false,
}

// judgeSystemPrompt tells Claude how to judge a shortlist: the definition of
// strongly related from CONTEXT.md, the PRD's case of a question and its
// answer, and made-up examples. It gives the Thoughts' texts only, no Kinds,
// Topics or dates (ADR-0010).
const judgeSystemPrompt = `You judge whether past Thoughts are strongly related to a new one. A Thought is a short message someone sent to themselves, in English, Bahasa Indonesia or a mix of the two.

Two Thoughts are strongly related when they are about the same specific subject, in any language, not merely sharing a Topic: a broad area that many Thoughts share, such as work, health or coding. A question and its later answer are strongly related.

Examples, made up for this prompt:
- Strongly related, in two languages: "Idea: a bot that waters the plants on a schedule." and "Kalau besok hujan, bot penyiram tanaman harusnya skip jadwal siram."
- Not strongly related, only sharing an area: "Go generics feel clunky for collections." and "Rust's borrow checker finally taught me ownership." Both are about coding, but not about the same thing.
- Strongly related, a question and its answer: "Why does my sourdough always come out flat?" and "Ternyata starter-nya masih terlalu muda. Pakai yang umurnya seminggu, rotinya mengembang."

The user message gives the new Thought, then the past Thoughts, numbered. For each past Thought, in order, give your own estimate, from 0 to 1, of how likely it is that it and the new Thought are strongly related: one confidence per past Thought.`

// renderJudgeInput writes the user message of a judge call: the new Thought's
// text, then each shortlisted Thought's, numbered from 1.
func renderJudgeInput(text string, shortlist []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<new_thought>\n%s\n</new_thought>\n", text)
	for i, past := range shortlist {
		fmt.Fprintf(&b, "<past_thought number=\"%d\">\n%s\n</past_thought>\n", i+1, past)
	}
	return b.String()
}
