package cermin_test

import (
	"math"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// thoughtAt is when the new Thought in the Resurfacing tests was sent.
var thoughtAt = time.Date(2026, 10, 16, 1, 30, 0, 0, time.UTC)

// newThought is the Thought just captured, at thoughtAt, from Telegram
// message 70, replying to message replyTo, or to none if it is 0.
func newThought(replyTo int64) cermin.EmbeddedThought {
	return cermin.EmbeddedThought{
		Thought: cermin.Thought{ID: 10, TGMessageID: 70, TGReplyToMessageID: replyTo, ThoughtAt: thoughtAt},
		Vector:  []float32{1, 0},
	}
}

// pastThought is a Thought sent ago before thoughtAt with vector, and with no
// Telegram message, as an imported Thought has.
func pastThought(id int64, ago time.Duration, vector ...float32) cermin.EmbeddedThought {
	return cermin.EmbeddedThought{
		Thought: cermin.Thought{ID: id, ThoughtAt: thoughtAt.Add(-ago)},
		Vector:  vector,
	}
}

// Imported Thoughts have no message, so message IDs of 0 that are equal never
// count as a reply, and neither does a reply to a message no Thought has.
func TestRankEligible_RanksByScore(t *testing.T) {
	t.Parallel()
	fromTelegram := pastThought(3, 24*time.Hour+time.Minute, 1, 0)
	fromTelegram.TGMessageID, fromTelegram.AckMessageID = 60, 61
	corpus := []cermin.EmbeddedThought{
		pastThought(1, 48*time.Hour, 0, 1),
		pastThought(2, 72*time.Hour, 1, 1),
		fromTelegram,
	}
	want := []cermin.ScoredThought{
		{Thought: corpus[2].Thought, Score: 1},
		{Thought: corpus[1].Thought, Score: math.Sqrt2 / 2},
		{Thought: corpus[0].Thought, Score: 0},
	}
	tests := []struct {
		name    string
		replyTo int64
	}{
		{name: "replying to none", replyTo: 0},
		{name: "replying to another message", replyTo: 99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cermin.RankEligible(newThought(tt.replyTo), corpus)

			if len(got) != len(want) {
				t.Fatalf("RankEligible = %+v, want %+v", got, want)
			}
			for i := range want {
				if got[i].ID != want[i].ID || math.Abs(got[i].Score-want[i].Score) > 1e-9 {
					t.Errorf(
						"RankEligible[%d] = Thought %d scoring %v, want Thought %d scoring %v",
						i,
						got[i].ID,
						got[i].Score,
						want[i].ID,
						want[i].Score,
					)
				}
			}
		})
	}
}

// Thoughts you still remember never come back: those from the last 24 hours,
// and the one the new Thought replies to.
func TestRankEligible_SkipsRemembered(t *testing.T) {
	t.Parallel()
	thought := newThought(50)
	repliedThrough := func(messageID, ackID int64) cermin.EmbeddedThought {
		past := pastThought(1, 48*time.Hour, 1, 0)
		past.TGMessageID, past.AckMessageID = messageID, ackID
		return past
	}
	tests := []struct {
		name string
		past cermin.EmbeddedThought
	}{
		{name: "less than 24 h earlier", past: pastThought(1, 23*time.Hour, 1, 0)},
		{name: "exactly 24 h earlier", past: pastThought(1, 24*time.Hour, 1, 0)},
		{name: "later", past: pastThought(1, -time.Hour, 1, 0)},
		{name: "itself", past: thought},
		{name: "replied to through its message", past: repliedThrough(50, 51)},
		{name: "replied to through its Acknowledgement", past: repliedThrough(49, 50)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cermin.RankEligible(thought, []cermin.EmbeddedThought{tt.past}); len(got) != 0 {
				t.Errorf("RankEligible = %+v, want none", got)
			}
		})
	}
}

func TestMeasureSimilarity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b []float32
		want float64
	}{
		{name: "identical", a: []float32{0.3, -0.5, 0.8}, b: []float32{0.3, -0.5, 0.8}, want: 1},
		{name: "same direction, longer", a: []float32{1, 2}, b: []float32{3, 6}, want: 1},
		{name: "orthogonal", a: []float32{1, 0}, b: []float32{0, 2}, want: 0},
		{name: "opposite", a: []float32{1, -1}, b: []float32{-1, 1}, want: -1},
		{name: "all zeros", a: []float32{0, 0}, b: []float32{1, 1}, want: 0},
		{name: "different lengths", a: []float32{1, 0}, b: []float32{1, 0, 0}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cermin.MeasureSimilarity(tt.a, tt.b); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("MeasureSimilarity(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
