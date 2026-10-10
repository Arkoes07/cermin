package cermin_test

import (
	"reflect"
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

func TestClassification_CorrectKind(t *testing.T) {
	t.Parallel()
	kinds := []cermin.Kind{{Name: "idea"}, {Name: "plan"}}
	current := cermin.Classification{
		ID:             1,
		ThoughtID:      9,
		Source:         cermin.SourceCermin,
		Kind:           "idea",
		KindConfidence: 0.9,
		Topics: []cermin.TopicCandidate{
			{Name: "running", Confidence: 0.9, Kept: true},
			{Name: "health", Confidence: 0.4},
			{Name: "habits", Confidence: 0.8, Kept: true},
		},
		Model: "claude-test",
	}
	tests := []struct {
		name      string
		kind      string
		want      cermin.Classification
		wantValid bool
	}{
		{
			name: "kept topics copied with no confidence",
			kind: " Plan ",
			want: cermin.Classification{
				ThoughtID: 9,
				Source:    cermin.SourceCorrection,
				Kind:      "plan",
				Topics:    []cermin.TopicCandidate{{Name: "running", Kept: true}, {Name: "habits", Kept: true}},
			},
			wantValid: true,
		},
		{name: "unknown kind", kind: "rant"},
		{name: "no kind", kind: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, isValid := current.CorrectKind(tt.kind, kinds)
			if isValid != tt.wantValid || !reflect.DeepEqual(got, tt.want) {
				t.Errorf("CorrectKind(%q) = %+v, %t; want %+v, %t", tt.kind, got, isValid, tt.want, tt.wantValid)
			}
		})
	}
}

func TestClassification_CorrectTopics(t *testing.T) {
	t.Parallel()
	current := cermin.Classification{
		ID:             1,
		ThoughtID:      9,
		Source:         cermin.SourceCermin,
		Kind:           "plan",
		KindConfidence: 0.9,
		Topics:         []cermin.TopicCandidate{{Name: "running", Confidence: 0.9, Kept: true}},
		Model:          "claude-test",
	}
	tests := []struct {
		name       string
		names      []string
		wantTopics []string // nil if rejected
	}{
		{
			name:       "trimmed and lowercased, in your order",
			names:      []string{" Weekend", "Running "},
			wantTopics: []string{"weekend", "running"},
		},
		{
			name:       "blank and repeated dropped",
			names:      []string{"health", "", " ", "Health", "habits"},
			wantTopics: []string{"health", "habits"},
		},
		{
			name:       "3 after repeats dropped",
			names:      []string{"a", "b", "A", "c"},
			wantTopics: []string{"a", "b", "c"},
		},
		{name: "none", names: []string{" ", ""}},
		{name: "4", names: []string{"a", "b", "c", "d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, isValid := current.CorrectTopics(tt.names)
			if tt.wantTopics == nil {
				if isValid {
					t.Errorf("CorrectTopics(%q) = %+v, want it rejected", tt.names, got)
				}
				return
			}
			want := cermin.Classification{
				ThoughtID: 9,
				Source:    cermin.SourceCorrection,
				Kind:      "plan",
				Topics:    []cermin.TopicCandidate{},
			}
			for _, name := range tt.wantTopics {
				want.Topics = append(want.Topics, cermin.TopicCandidate{Name: name, Kept: true})
			}
			if !isValid || !reflect.DeepEqual(got, want) {
				t.Errorf("CorrectTopics(%q) = %+v, %t; want %+v", tt.names, got, isValid, want)
			}
		})
	}
}
