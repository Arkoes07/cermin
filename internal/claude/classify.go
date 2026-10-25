// Package claude calls the Claude API for Cermin. A Classifier gives a Thought
// its Classification: one call that names its Kind and candidate Topics, each
// with Claude's stated confidence. A Judge states, in one call, how likely each
// past Thought shortlisted for a Resurfacing is to be strongly related to the
// new one.
package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/arkoes07/cermin/internal/cermin"
)

// DefaultModel is the Claude model that classifies unless configured
// otherwise.
const DefaultModel = "claude-sonnet-5-5"

const (
	// promptVersion names the prompt buildSystemPrompt writes. Change it with
	// the prompt, or with the Kinds, so accuracy can be reported per version.
	promptVersion = "classify-v1"
	// confidenceMethod is how the confidences are measured: Claude states them.
	confidenceMethod = "verbalized-v1"
	// maxTokens leaves room for the output and any thinking at low effort.
	maxTokens = 4096
	// purposeClassify labels the metrics of classification calls.
	purposeClassify = "classify"
)

// Every Claude API call is recorded in these, by what it was for and the
// model it asked. A call the SDK retries counts once, and its duration takes
// in the retries.
var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cermin_claude_requests_total",
		Help: "Claude API calls, by purpose, model and outcome: ok or error.",
	}, []string{"purpose", "model", "outcome"})
	// The buckets reach past the 8 s an Acknowledgement waits.
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cermin_claude_request_duration_seconds",
		Help:    "How long Claude API calls took, by purpose and model.",
		Buckets: []float64{0.5, 1, 2, 4, 8, 16, 32},
	}, []string{"purpose", "model"})
	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "cermin_claude_tokens_total",
		Help: "Tokens billed for Claude API calls, by purpose, model and direction: input or output.",
	}, []string{"purpose", "model", "direction"})
)

// Classifier classifies Thoughts with a Claude model.
type Classifier struct {
	client anthropic.Client
	model  string
}

// New returns a Classifier that calls model with the Anthropic API key apiKey.
func New(apiKey, model string) *Classifier {
	initCallMetrics(purposeClassify, model)
	return &Classifier{client: anthropic.NewClient(option.WithAPIKey(apiKey)), model: model}
}

// initCallMetrics creates the metrics of calls made for purpose to model at
// 0, so that increase() counts the first call after a restart.
func initCallMetrics(purpose, model string) {
	requestDuration.WithLabelValues(purpose, model)
	for _, outcome := range []string{"ok", "error"} {
		requestsTotal.WithLabelValues(purpose, model, outcome)
	}
	for _, direction := range []string{"input", "output"} {
		tokensTotal.WithLabelValues(purpose, model, direction)
	}
}

// Classify asks Claude for the Classification of a Thought's text: one of
// kinds, 3 to 5 candidate Topics that reuse topics where one fits, and the
// Thought's language: en, id or mixed. The candidates come back as Claude
// gave them, none of them kept; cermin.KeepTopics picks those.
func (c *Classifier) Classify(
	ctx context.Context,
	text string,
	kinds []cermin.Kind,
	topics []string,
) (_ cermin.Classification, err error) {
	start := time.Now()
	resp, err := c.client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     c.model,
		MaxTokens: maxTokens,
		System:    []anthropic.TextBlockParam{{Text: buildSystemPrompt(kinds, topics)}},
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock(text))},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: anthropic.OutputConfigEffortLow,
			Format: anthropic.JSONOutputFormatParam{Schema: buildOutputSchema(kinds)},
		},
	})
	elapsed := time.Since(start)
	// Recorded as Classify returns, so that output that fails to parse counts
	// as an error.
	defer func() { recordCall(purposeClassify, c.model, elapsed, resp, err) }()
	if err != nil {
		return cermin.Classification{}, fmt.Errorf("classify: %w", err)
	}
	// A refused or cut-off output may not match the schema.
	if resp.StopReason != anthropic.StopReasonEndTurn {
		return cermin.Classification{}, fmt.Errorf("classify: model stopped with %s", resp.StopReason)
	}

	raw := readOutput(resp)
	var out output
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return cermin.Classification{}, fmt.Errorf("classify: decoding output: %w", err)
	}

	candidates := make([]cermin.TopicCandidate, 0, len(out.Topics))
	for _, t := range out.Topics {
		candidates = append(candidates, cermin.TopicCandidate{Name: t.Name, Confidence: t.Confidence})
	}

	classification := cermin.Classification{
		Kind:             out.Kind.Value,
		KindConfidence:   out.Kind.Confidence,
		Topics:           candidates,
		Model:            resp.Model,
		PromptVersion:    promptVersion,
		ConfidenceMethod: confidenceMethod,
		RawOutput:        raw,
		Language:         out.Language,
	}
	return classification, nil
}

// readOutput returns the text of resp's first text block: the output, in the
// shape the call's schema gives it.
func readOutput(resp *anthropic.Message) string {
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}

// recordCall records a call to model made for purpose that took elapsed and
// ended in err: its outcome, its duration and, if resp arrived, the tokens
// billed for it.
func recordCall(purpose, model string, elapsed time.Duration, resp *anthropic.Message, err error) {
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	requestsTotal.WithLabelValues(purpose, model, outcome).Inc()
	requestDuration.WithLabelValues(purpose, model).Observe(elapsed.Seconds())
	if resp != nil {
		tokensTotal.WithLabelValues(purpose, model, "input").Add(float64(resp.Usage.InputTokens))
		tokensTotal.WithLabelValues(purpose, model, "output").Add(float64(resp.Usage.OutputTokens))
	}
}

// output is the model's output, as buildOutputSchema shapes it. Its
// kind_suggestion stays in the raw output only.
type output struct {
	Language string `json:"language"`
	Kind     struct {
		Value      string  `json:"value"`
		Confidence float64 `json:"confidence"`
	} `json:"kind"`
	Topics []struct {
		Name       string  `json:"name"`
		Confidence float64 `json:"confidence"`
	} `json:"topics"`
}

// buildOutputSchema returns the JSON schema Claude's output must follow, with
// the Kind limited to kinds.
func buildOutputSchema(kinds []cermin.Kind) map[string]any {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"language": map[string]any{"type": "string", "enum": []string{"en", "id", "mixed"}},
			"kind": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value":      map[string]any{"type": "string", "enum": names},
					"confidence": map[string]any{"type": "number"},
				},
				"required":             []string{"value", "confidence"},
				"additionalProperties": false,
			},
			"topics": map[string]any{
				"type":     "array",
				"minItems": 1,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":       map[string]any{"type": "string"},
						"confidence": map[string]any{"type": "number"},
					},
					"required":             []string{"name", "confidence"},
					"additionalProperties": false,
				},
			},
			"kind_suggestion": map[string]any{"type": "string"},
		},
		"required":             []string{"language", "kind", "topics", "kind_suggestion"},
		"additionalProperties": false,
	}
}

// buildSystemPrompt writes the system prompt that tells Claude how to classify
// a Thought, giving it the definitions of kinds and the names of the existing
// topics.
func buildSystemPrompt(kinds []cermin.Kind, topics []string) string {
	var b strings.Builder
	b.WriteString(`You classify a Thought: a short message someone sent to themselves, in English, Bahasa Indonesia or a mix of the two. The user message is the Thought. In the definitions below, "I" is the person who wrote it.

Kind: the shape of the Thought. Give exactly one, the closest fit:
`)
	for _, k := range kinds {
		fmt.Fprintf(&b, "- %s: %s\n", k.Name, k.Definition)
	}
	b.WriteString(`If none fits well, still give the closest one, and put the Kind you would have named in kind_suggestion. Otherwise leave kind_suggestion empty.

Topics: the broad areas a Thought is about, which many Thoughts share. Examples of the right breadth, not a list to pick from: coding, business, philosophy, work, job hunt, health.
A Topic never names the Thought's shape, its Kind: a "business idea" is Kind idea with Topic business.
A Thought is often about more than one area. Give 3 to 5 candidates, best first, and judge each on its own: a second or third area the Thought is about is as right as the first. The existing Topics:
`)
	if len(topics) == 0 {
		b.WriteString("(none yet)\n")
	}
	for _, t := range topics {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	b.WriteString(`Reuse an existing Topic whenever one fits, rather than creating a near-duplicate of it. A new Topic's name is short, one or two words, English and lowercase, even when the Thought isn't in English.

Confidence: for the Kind and for each Topic, give your own estimate, from 0 to 1, of how likely it is to be right.

Language: en if the Thought is in English, id if it is in Bahasa Indonesia, mixed if it uses both.`)
	return b.String()
}
