package cermin

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

const (
	// keepThreshold is the confidence a candidate Topic needs to be kept, a
	// placeholder until week 3 measures calibration.
	keepThreshold = 0.7
	// maxKept is the most Topics a Thought keeps.
	maxKept = 3
)

// Kind is a shape a Thought can have.
type Kind struct {
	Name       string
	Emoji      string
	Definition string // what it means, as the classifier's prompt gives it
}

// Classification is the Kind and Topics a Thought was given at one moment, by
// Cermin or in a Correction.
type Classification struct {
	ID               int64
	ThoughtID        int64
	Source           string // "cermin", "correction" or "declared"
	Kind             string
	KindConfidence   float64
	Topics           []TopicCandidate // every candidate Topic, best first
	Model            string           // the model that classified; "" for a Correction
	PromptVersion    string
	ConfidenceMethod string // how the confidences were measured
	RawOutput        string // the model's output, verbatim
	TGMessageID      int64  // the Correction's Telegram message; 0 if none
	CreatedAt        time.Time
}

// TopicCandidate is a Topic considered for a Classification. A kept one is
// assigned to the Thought; the others were considered and dropped.
type TopicCandidate struct {
	Name       string
	Confidence float64
	Kept       bool
}

// KeepTopics returns candidates best first, with the ones the Thought keeps
// marked Kept: those at keepThreshold or above, at most maxKept of them, or
// else the single best. Names are trimmed and lowercased first, and a blank or
// repeated name is dropped, so no candidate names a Topic twice.
func KeepTopics(candidates []TopicCandidate) []TopicCandidate {
	sorted := make([]TopicCandidate, 0, len(candidates))
	for _, c := range candidates {
		c.Name = strings.ToLower(strings.TrimSpace(c.Name))
		if c.Name != "" {
			sorted = append(sorted, c)
		}
	}
	slices.SortStableFunc(sorted, func(a, b TopicCandidate) int {
		return cmp.Compare(b.Confidence, a.Confidence)
	})

	unique := make([]TopicCandidate, 0, len(sorted))
	for _, c := range sorted {
		isRepeat := slices.ContainsFunc(unique, func(u TopicCandidate) bool { return u.Name == c.Name })
		if isRepeat {
			continue
		}
		c.Kept = len(unique) < maxKept && c.Confidence >= keepThreshold
		unique = append(unique, c)
	}
	if len(unique) > 0 {
		unique[0].Kept = true // the best is kept even below the threshold
	}
	return unique
}
