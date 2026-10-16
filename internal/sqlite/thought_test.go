package sqlite

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// A second import of the same export, or of one that stopped halfway, must
// add nothing.
func TestDB_SaveImportedThought_KeepsOne(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	imported := cermin.Thought{
		Text:       "Maybe the morning walk is when ideas come, not the desk.",
		Source:     "whatsapp",
		ThoughtAt:  time.Date(2026, 10, 3, 12, 10, 30, 0, time.UTC),
		ReceivedAt: time.Now(),
	}

	for i, wantNew := range []bool{true, false} {
		isNew, err := db.SaveImportedThought(ctx, imported)
		if err != nil || isNew != wantNew {
			t.Fatalf("SaveImportedThought #%d = %t, %v; want %t", i+1, isNew, err, wantNew)
		}
	}
	thoughts, err := db.GetThoughts(ctx)
	if err != nil || len(thoughts) != 1 {
		t.Fatalf("GetThoughts = %+v, %v; want 1", thoughts, err)
	}
	want := imported
	want.ID, want.ReceivedAt = thoughts[0].ID, thoughts[0].ReceivedAt
	if thoughts[0] != want {
		t.Errorf("stored %+v, want %+v", thoughts[0], want)
	}
}

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
	if err := db.DeleteThought(ctx, deletedThought.ID, 0); err != nil {
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

// Litestream copies the file's pages to R2, and retention ages out only the
// copies made before a delete, so the deleted text must leave the file's
// pages too, not just the table.
func TestDB_DeleteThought_ZeroesText(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "cermin.db")
	db := openDBAt(t, path)

	thought := saveThought(t, db)
	if err := db.DeleteThought(ctx, thought.ID, 0); err != nil {
		t.Fatalf("DeleteThought: %v", err)
	}
	// Moves every page into the file and empties the WAL.
	var isBusy bool
	var walPages, movedPages int
	err := db.pool.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&isBusy, &walPages, &movedPages)
	if err != nil || isBusy {
		t.Fatalf("checkpoint: busy %t, %v", isBusy, err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if bytes.Contains(content, []byte(thought.Text)) {
		t.Errorf("the file still holds the deleted Thought's text")
	}
}
