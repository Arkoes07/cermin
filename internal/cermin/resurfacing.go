package cermin

import (
	"cmp"
	"math"
	"slices"
	"time"
)

// rememberWindow is how long you still remember a Thought, so that it can't
// come back in a Resurfacing.
const rememberWindow = 24 * time.Hour

// ScoredThought is a past Thought with its score against a new one: the
// cosine similarity of their vectors.
type ScoredThought struct {
	Thought
	Score float64
}

// RankEligible returns the Thoughts in corpus that are eligible to come back
// in a Resurfacing of t, scored against it, best first. Thoughts you still
// remember are never eligible: those up to rememberWindow before t, t itself
// and later ones, and the one t replies to, through its message or its
// Acknowledgement.
func RankEligible(t EmbeddedThought, corpus []EmbeddedThought) []ScoredThought {
	ranked := []ScoredThought{}
	for _, past := range corpus {
		if t.ThoughtAt.Sub(past.ThoughtAt) <= rememberWindow || t.repliesTo(past.Thought) {
			continue
		}
		ranked = append(ranked, ScoredThought{Thought: past.Thought, Score: MeasureSimilarity(t.Vector, past.Vector)})
	}
	slices.SortStableFunc(ranked, func(a, b ScoredThought) int {
		return cmp.Compare(b.Score, a.Score)
	})
	return ranked
}

// repliesTo reports whether t replies to past, through its message or its
// Acknowledgement. A message ID of 0 is no message, as with a Thought that
// replies to none, or an imported one.
func (t Thought) repliesTo(past Thought) bool {
	reply := t.TGReplyToMessageID
	return reply != 0 && (reply == past.TGMessageID || reply == past.AckMessageID)
}

// MeasureSimilarity returns the cosine similarity of vectors a and b: 1 when
// they point the same way, 0 when they are orthogonal. It is 0 if either is
// all zeros or their lengths differ, as vectors from two models do: such
// vectors are related to nothing.
func MeasureSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / math.Sqrt(normA*normB)
}
