package sqlite

import (
	"testing"

	"github.com/arkoes07/cermin/internal/cermin"
)

// Logs name a Thought or a Classification by its ID alone, so a deleted one's
// ID must never go to a new one. The Thought deleted is usually the newest,
// holding the highest IDs.
func TestDB_DeleteThought_KeepsIDsUnused(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	correction := cermin.Classification{
		Source: cermin.SourceCorrection,
		Kind:   "plan",
		Topics: []cermin.TopicCandidate{{Name: "running", Kept: true}},
	}

	deletedThought := saveThought(t, db)
	correction.ThoughtID = deletedThought.ID
	deletedClassification, err := db.SaveClassification(ctx, correction)
	if err != nil {
		t.Fatalf("SaveClassification: %v", err)
	}
	if err := db.DeleteThought(ctx, deletedThought.ID); err != nil {
		t.Fatalf("DeleteThought: %v", err)
	}

	newThought := saveThought(t, db)
	correction.ThoughtID = newThought.ID
	newClassification, err := db.SaveClassification(ctx, correction)
	if err != nil {
		t.Fatalf("SaveClassification: %v", err)
	}
	if newThought.ID <= deletedThought.ID {
		t.Errorf("new Thought's ID = %d, want above the deleted one's, %d", newThought.ID, deletedThought.ID)
	}
	if newClassification.ID <= deletedClassification.ID {
		t.Errorf("new Classification's ID = %d, want above the deleted one's, %d",
			newClassification.ID, deletedClassification.ID)
	}
}
