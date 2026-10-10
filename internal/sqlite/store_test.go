package sqlite

import (
	"database/sql"
	"path/filepath"
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
	}, "en")
	if err != nil {
		t.Fatalf("SaveClassification of Cermin's: %v", err)
	}

	// A Topic whose stored name isn't lowercase, as a Declared Topic's may be.
	now := formatTime(time.Now())
	_, err = db.pool.ExecContext(ctx,
		`INSERT INTO topics (name, origin, created_at, declared_at) VALUES ('Cermin', 'declared', ?, ?)`,
		now,
		now,
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
	}, "")
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

func TestDB_CurrentClassification(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)

	_, isClassified, err := db.CurrentClassification(ctx, thought.ID)
	if err != nil || isClassified {
		t.Fatalf("CurrentClassification of an unclassified Thought: %t, %v; want false", isClassified, err)
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
		want, err := db.SaveClassification(ctx, c, "")
		if err != nil {
			t.Fatalf("SaveClassification %d: %v", i, err)
		}
		got, isClassified, err := db.CurrentClassification(ctx, thought.ID)
		if err != nil || !isClassified || !reflect.DeepEqual(got, want) {
			t.Errorf("after saving %d, CurrentClassification = %+v, %t, %v; want %+v", i, got, isClassified, err, want)
		}
	}
}

// openDB opens a new database in a temporary directory.
func openDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.Context(), filepath.Join(t.TempDir(), "cermin.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return db
}

// saveThought saves a Thought from Telegram message 7 in db and returns it.
func saveThought(t *testing.T, db *DB) cermin.Thought {
	t.Helper()
	thought, err := db.SaveThought(t.Context(), cermin.Thought{
		Text:        "Start running on Saturdays.",
		Source:      "telegram",
		TGMessageID: 7,
		ThoughtAt:   time.Now(),
		ReceivedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("SaveThought: %v", err)
	}
	return thought
}
