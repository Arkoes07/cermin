package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/chatexport"
	"github.com/arkoes07/cermin/internal/sqlite"
)

// importChat saves each message in the WhatsApp chat export at path as a
// Thought, with WhatsApp as its source, the channel it was captured in. It
// skips those already imported, so it can be run again, and prints how many it
// imported and skipped.
func importChat(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	messages, err := chatexport.Parse(f)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	now := time.Now()
	imported := 0
	for _, m := range messages {
		isNew, err := db.SaveImportedThought(ctx, cermin.Thought{
			Text:       m.Text,
			Source:     "whatsapp",
			ThoughtAt:  m.ThoughtAt,
			ReceivedAt: now,
		})
		if err != nil {
			return errors.Join(err, db.Close())
		}
		if isNew {
			imported++
		}
	}
	fmt.Printf("imported %d Thoughts, skipped %d already imported\n", imported, len(messages)-imported)
	return db.Close()
}
