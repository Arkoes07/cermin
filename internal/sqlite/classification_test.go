package sqlite

import (
	"database/sql"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// Week 3 measures accuracy and calibration with SQL on a copy of the
// database, so how a Correction is stored is tested on the columns
// themselves.
func TestDB_SaveClassification_StoresCorrection(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)
	_, err := db.SaveClassification(ctx, cermin.Classification{
		ThoughtID:        thought.ID,
		Source:           cermin.SourceCermin,
		Kind:             "idea",
		KindConfidence:   0.9,
		Topics:           []cermin.TopicCandidate{{Name: "running", Confidence: 0.9, Kept: true}},
		Model:            "claude-test",
		PromptVersion:    "classify-v1",
		ConfidenceMethod: "verbalized-v1",
		RawOutput:        "{}",
		Language:         "en",
	})
	if err != nil {
		t.Fatalf("SaveClassification of Cermin's: %v", err)
	}

	// A Topic whose stored name isn't lowercase, as a Declared Topic's may be.
	now := formatTime(time.Now())
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO topics (name, origin, created_at, declared_at) VALUES ('Cermin', 'declared', :now, :now)`,
		sql.Named("now", now),
	)
	if err != nil {
		t.Fatalf("declaring a topic: %v", err)
	}

	correction, err := db.SaveClassification(ctx, cermin.Classification{
		ThoughtID: thought.ID,
		Source:    cermin.SourceCorrection,
		Kind:      "plan",
		Topics: []cermin.TopicCandidate{
			{Name: "running", Kept: true},
			{Name: "health", Kept: true},
			{Name: "cermin", Kept: true},
		},
		TGMessageID: 20,
	})
	if err != nil {
		t.Fatalf("SaveClassification of the Correction: %v", err)
	}
	if got, want := correction.KeptTopics(), []string{"running", "health", "Cermin"}; !slices.Equal(got, want) {
		t.Errorf("Correction's Topics = %q, want %q, with the stored spelling", got, want)
	}

	var kindConfidence sql.Null[float64]
	err = db.pool.QueryRowContext(ctx,
		`SELECT kind_confidence FROM classifications WHERE id = ?`,
		correction.ID,
	).Scan(&kindConfidence)
	if err != nil || kindConfidence.Valid {
		t.Errorf("kind_confidence = %+v, %v; want NULL", kindConfidence, err)
	}
	var language sql.Null[string]
	err = db.pool.QueryRowContext(ctx,
		`SELECT language FROM classifications WHERE id = ?`,
		correction.ID,
	).Scan(&language)
	if err != nil || language.Valid {
		t.Errorf("language = %+v, %v; want NULL", language, err)
	}
	var stated int
	err = db.pool.QueryRowContext(ctx,
		`SELECT COUNT(confidence) FROM classification_topics WHERE classification_id = ?`,
		correction.ID,
	).Scan(&stated)
	if err != nil || stated != 0 {
		t.Errorf("%d Topic confidences stated, %v; want all NULL", stated, err)
	}
	rows, err := db.pool.QueryContext(ctx, `SELECT name || ' ' || origin FROM topics ORDER BY name`)
	if err != nil {
		t.Fatalf("listing topics: %v", err)
	}
	defer rows.Close()
	topicOrigins := []string{} // each "<name> <origin>"
	for rows.Next() {
		var topicOrigin string
		if err := rows.Scan(&topicOrigin); err != nil {
			t.Fatalf("reading topic: %v", err)
		}
		topicOrigins = append(topicOrigins, topicOrigin)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing topics: %v", err)
	}
	if want := []string{"Cermin declared", "health you", "running cermin"}; !slices.Equal(topicOrigins, want) {
		t.Errorf("topics = %q, want %q", topicOrigins, want)
	}
}

func TestDB_SaveFirstClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		earlier   *cermin.Classification // saved first with SaveClassification, if set
		isDeleted bool                   // the Thought is deleted first
		wantSaved bool
	}{
		{
			name:      "unclassified",
			wantSaved: true,
		},
		{
			name: "classified by Cermin",
			earlier: &cermin.Classification{
				Source:           cermin.SourceCermin,
				Kind:             "plan",
				KindConfidence:   0.8,
				Topics:           []cermin.TopicCandidate{{Name: "running", Confidence: 0.8, Kept: true}},
				Model:            "claude-test",
				PromptVersion:    "classify-v1",
				ConfidenceMethod: "verbalized-v1",
				RawOutput:        "{}",
				Language:         "en",
			},
		},
		{
			// Or a Declared Topic backfill's, which can come first.
			name: "with only a Correction",
			earlier: &cermin.Classification{
				Source: cermin.SourceCorrection,
				Kind:   "plan",
				Topics: []cermin.TopicCandidate{{Name: "running", Kept: true}},
			},
			wantSaved: true,
		},
		{
			name:      "deleted",
			isDeleted: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			db := openDB(t)
			thought := saveThought(t, db)
			want := []cermin.Classification{}
			if tt.earlier != nil {
				earlier := *tt.earlier
				earlier.ThoughtID = thought.ID
				stored, err := db.SaveClassification(ctx, earlier)
				if err != nil {
					t.Fatalf("SaveClassification: %v", err)
				}
				want = append(want, stored)
			}
			if tt.isDeleted {
				if err := db.DeleteThought(ctx, thought.ID); err != nil {
					t.Fatalf("DeleteThought: %v", err)
				}
			}

			stored, isSaved, err := db.SaveFirstClassification(ctx, cermin.Classification{
				ThoughtID:        thought.ID,
				Source:           cermin.SourceCermin,
				Kind:             "idea",
				KindConfidence:   0.9,
				Topics:           []cermin.TopicCandidate{{Name: "errands", Confidence: 0.9, Kept: true}},
				Model:            "claude-test",
				PromptVersion:    "classify-v1",
				ConfidenceMethod: "verbalized-v1",
				RawOutput:        "{}",
				Language:         "id",
			})
			if err != nil {
				t.Fatalf("SaveFirstClassification: %v", err)
			}
			if isSaved != tt.wantSaved {
				t.Errorf("SaveFirstClassification saved: %t, want %t", isSaved, tt.wantSaved)
			}
			if isSaved {
				want = append(want, stored)
			} else if !reflect.DeepEqual(stored, cermin.Classification{}) {
				t.Errorf("SaveFirstClassification returned %+v unsaved, want none", stored)
			}

			got, err := db.GetClassificationsByThoughtID(ctx, thought.ID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Errorf("GetClassificationsByThoughtID = %+v, %v; want %+v", got, err, want)
			}
			topics, err := db.GetTopicNames(ctx)
			if err != nil {
				t.Fatalf("GetTopicNames: %v", err)
			}
			if hasErrands := slices.Contains(topics, "errands"); hasErrands != tt.wantSaved {
				t.Errorf("GetTopicNames = %q, want errands only if saved", topics)
			}
		})
	}
}

func TestDB_GetCurrentClassificationByThoughtID(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)

	_, isClassified, err := db.GetCurrentClassificationByThoughtID(ctx, thought.ID)
	if err != nil || isClassified {
		t.Fatalf(
			"GetCurrentClassificationByThoughtID of an unclassified Thought: %t, %v; want false",
			isClassified,
			err,
		)
	}

	// Each one saved is current, with its own candidate Topics.
	classifications := []cermin.Classification{
		{
			ThoughtID:      thought.ID,
			Source:         cermin.SourceCermin,
			Kind:           "idea",
			KindConfidence: 0.9,
			Topics: []cermin.TopicCandidate{
				{Name: "running", Confidence: 0.9, Kept: true},
				{Name: "weekend", Confidence: 0.4},
			},
			Model:            "claude-test",
			PromptVersion:    "classify-v1",
			ConfidenceMethod: "verbalized-v1",
			RawOutput:        "{}",
			Language:         "id",
		},
		{
			ThoughtID:   thought.ID,
			Source:      cermin.SourceCorrection,
			Kind:        "plan",
			Topics:      []cermin.TopicCandidate{{Name: "running", Kept: true}},
			TGMessageID: 20,
		},
		{
			ThoughtID:   thought.ID,
			Source:      cermin.SourceCorrection,
			Kind:        "plan",
			Topics:      []cermin.TopicCandidate{{Name: "health", Kept: true}, {Name: "habits", Kept: true}},
			TGMessageID: 21,
		},
	}
	for i, c := range classifications {
		want, err := db.SaveClassification(ctx, c)
		if err != nil {
			t.Fatalf("SaveClassification %d: %v", i, err)
		}
		got, isClassified, err := db.GetCurrentClassificationByThoughtID(ctx, thought.ID)
		if err != nil || !isClassified || !reflect.DeepEqual(got, want) {
			t.Errorf(
				"after saving %d, GetCurrentClassificationByThoughtID = %+v, %t, %v; want %+v",
				i,
				got,
				isClassified,
				err,
				want,
			)
		}
	}
}
