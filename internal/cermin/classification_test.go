package cermin_test

import (
	"slices"
	"testing"

	"github.com/arkoes07/cermin/internal/cermin"
)

func TestKeepTopics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		candidates []cermin.TopicCandidate
		want       []cermin.TopicCandidate
	}{
		{
			name: "strong candidates kept, best first",
			candidates: []cermin.TopicCandidate{
				{Name: "cities", Confidence: 0.8},
				{Name: "transport", Confidence: 0.95},
				{Name: "design", Confidence: 0.5},
			},
			want: []cermin.TopicCandidate{
				{Name: "transport", Confidence: 0.95, Kept: true},
				{Name: "cities", Confidence: 0.8, Kept: true},
				{Name: "design", Confidence: 0.5},
			},
		},
		{
			name: "at most three kept",
			candidates: []cermin.TopicCandidate{
				{Name: "a", Confidence: 0.9},
				{Name: "b", Confidence: 0.8},
				{Name: "c", Confidence: 0.75},
				{Name: "d", Confidence: 0.7},
			},
			want: []cermin.TopicCandidate{
				{Name: "a", Confidence: 0.9, Kept: true},
				{Name: "b", Confidence: 0.8, Kept: true},
				{Name: "c", Confidence: 0.75, Kept: true},
				{Name: "d", Confidence: 0.7},
			},
		},
		{
			name: "best kept below the threshold",
			candidates: []cermin.TopicCandidate{
				{Name: "retail", Confidence: 0.4},
				{Name: "business", Confidence: 0.6},
			},
			want: []cermin.TopicCandidate{
				{Name: "business", Confidence: 0.6, Kept: true},
				{Name: "retail", Confidence: 0.4},
			},
		},
		{
			name: "names normalized, blank and repeated dropped",
			candidates: []cermin.TopicCandidate{
				{Name: "  ", Confidence: 0.99},
				{Name: " Retail ", Confidence: 0.9},
				{Name: "retail", Confidence: 0.8},
			},
			want: []cermin.TopicCandidate{
				{Name: "retail", Confidence: 0.9, Kept: true},
			},
		},
		{
			name:       "no candidates",
			candidates: nil,
			want:       nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := cermin.KeepTopics(tt.candidates)
			if !slices.Equal(got, tt.want) {
				t.Errorf("KeepTopics(%+v) = %+v, want %+v", tt.candidates, got, tt.want)
			}
		})
	}
}
