package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

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
