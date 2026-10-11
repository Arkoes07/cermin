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
	keepThreshold = 0.5
	// maxKept is the most Topics a Thought keeps.
	maxKept = 3
)

// Kind is a shape a Thought can have.
type Kind struct {
	Name       string
	Emoji      string
	Definition string // what it means, as the classifier's prompt gives it
}

// Source is where a Classification came from.
type Source string

const (
	// SourceCermin is a Classification Cermin gave.
	SourceCermin Source = "cermin"
	// SourceCorrection is your Correction.
	SourceCorrection Source = "correction"
)

// Classification is the Kind and Topics a Thought was given at one moment, by
// Cermin or in a Correction.
type Classification struct {
	ID               int64
	ThoughtID        int64
	Source           Source
	Kind             string
	KindConfidence   float64          // 0 for a Correction, which states none
	Topics           []TopicCandidate // every candidate Topic, best first; a Correction's are all kept, in your order
	Model            string           // the model that classified; "" for a Correction
	PromptVersion    string
	ConfidenceMethod string // how the confidences were measured
	RawOutput        string // the model's output, verbatim
	Language         string // the Thought's, as the model detected it: en, id or mixed; "" for a Correction
	TGMessageID      int64  // the Correction's Telegram message; 0 if none
	CreatedAt        time.Time
}

// TopicCandidate is a Topic considered for a Classification. A kept one is
// assigned to the Thought; the others were considered and dropped.
type TopicCandidate struct {
	Name       string
	Confidence float64 // 0 in a Correction, which states none
	Kept       bool
}

// KeepTopics returns candidates best first, with the ones the Thought keeps
// marked Kept: those at keepThreshold or above, at most maxKept of them, or
// else the single best. Names are trimmed and lowercased first, and a blank or
// repeated name is dropped, so no candidate names a Topic twice.
func KeepTopics(candidates []TopicCandidate) []TopicCandidate {
	sorted := make([]TopicCandidate, 0, len(candidates))
	for _, c := range candidates {
		c.Name = lowerName(c.Name)
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

// KeptTopics returns the names of the Topics a Thought classified as c keeps,
// in order.
func (c Classification) KeptTopics() []string {
	names := []string{}
	for _, t := range c.Topics {
		if t.Kept {
			names = append(names, t.Name)
		}
	}
	return names
}

// CorrectKind returns your Correction of a Thought classified as c, which
// gives it the Kind named kind, in any case, and keeps its Topics. It returns
// false unless kind is one of kinds, since a Correction never adds a Kind. A
// Correction states no confidence.
func (c Classification) CorrectKind(kind string, kinds []Kind) (Classification, bool) {
	kind = lowerName(kind)
	if !slices.ContainsFunc(kinds, func(k Kind) bool { return k.Name == kind }) {
		return Classification{}, false
	}
	topics := []TopicCandidate{}
	for _, name := range c.KeptTopics() {
		topics = append(topics, TopicCandidate{Name: name, Kept: true})
	}
	return Classification{ThoughtID: c.ThoughtID, Source: SourceCorrection, Kind: kind, Topics: topics}, true
}

// CorrectTopics returns your Correction of a Thought classified as c, which
// gives it the Topics named names, in your order, and keeps its Kind. Names
// are trimmed and lowercased, and a blank or repeated name is dropped. It
// returns false unless 1 to maxKept Topics remain, as a Thought has. A
// Correction states no confidence.
func (c Classification) CorrectTopics(names []string) (Classification, bool) {
	topics := make([]TopicCandidate, 0, len(names))
	for _, name := range names {
		name = lowerName(name)
		isRepeat := slices.ContainsFunc(topics, func(t TopicCandidate) bool { return t.Name == name })
		if name == "" || isRepeat {
			continue
		}
		topics = append(topics, TopicCandidate{Name: name, Kept: true})
	}
	if len(topics) == 0 || len(topics) > maxKept {
		return Classification{}, false
	}
	return Classification{ThoughtID: c.ThoughtID, Source: SourceCorrection, Kind: c.Kind, Topics: topics}, true
}

// lowerName is name trimmed and lowercased, as Kind and Topic names are
// written, except a Declared Topic's.
func lowerName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
