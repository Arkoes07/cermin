package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// SaveThought stores a Thought taken from a Telegram message and returns it
// as stored. If that message's Thought is already stored, it is returned
// unchanged, so saving the same message twice keeps one Thought.
func (db *DB) SaveThought(ctx context.Context, t cermin.Thought) (cermin.Thought, error) {
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO thoughts (text, source, tg_message_id, tg_reply_to_message_id, thought_at, received_at)
		VALUES (?, ?, ?, NULLIF(?, 0), ?, ?)
		ON CONFLICT (tg_message_id) DO NOTHING`,
		t.Text,
		t.Source,
		t.TGMessageID,
		t.TGReplyToMessageID,
		formatTime(t.ThoughtAt),
		formatTime(t.ReceivedAt),
	)
	if err != nil {
		return cermin.Thought{}, fmt.Errorf("saving thought from message %d: %w", t.TGMessageID, err)
	}
	row := db.pool.QueryRowContext(ctx,
		`SELECT `+thoughtColumns+` FROM thoughts WHERE tg_message_id = ?`,
		t.TGMessageID,
	)
	stored, err := scanThought(row)
	if err != nil {
		return cermin.Thought{}, fmt.Errorf("reading thought from message %d: %w", t.TGMessageID, err)
	}
	return stored, nil
}

// SetAck records that message ackMessageID is a Thought's Acknowledgement and
// shows Classification classificationID, or none if it is 0.
func (db *DB) SetAck(ctx context.Context, thoughtID, ackMessageID, classificationID int64) error {
	_, err := db.pool.ExecContext(ctx,
		`UPDATE thoughts SET ack_message_id = ?, ack_classification_id = NULLIF(?, 0) WHERE id = ?`,
		ackMessageID,
		classificationID,
		thoughtID,
	)
	if err != nil {
		return fmt.Errorf("setting acknowledgement of thought %d: %w", thoughtID, err)
	}
	return nil
}

// Thought returns the Thought id, and false if there is none.
func (db *DB) Thought(ctx context.Context, id int64) (cermin.Thought, bool, error) {
	row := db.pool.QueryRowContext(ctx, `SELECT `+thoughtColumns+` FROM thoughts WHERE id = ?`, id)
	t, err := scanThought(row)
	if errors.Is(err, sql.ErrNoRows) {
		return cermin.Thought{}, false, nil
	}
	if err != nil {
		return cermin.Thought{}, false, fmt.Errorf("reading thought %d: %w", id, err)
	}
	return t, true, nil
}

// ThoughtByMessage returns the Thought that Telegram message messageID
// carried or acknowledged, and false if there is none.
func (db *DB) ThoughtByMessage(ctx context.Context, messageID int64) (cermin.Thought, bool, error) {
	row := db.pool.QueryRowContext(ctx,
		`SELECT `+thoughtColumns+` FROM thoughts WHERE tg_message_id = ? OR ack_message_id = ?`,
		messageID,
		messageID,
	)
	t, err := scanThought(row)
	if errors.Is(err, sql.ErrNoRows) {
		return cermin.Thought{}, false, nil
	}
	if err != nil {
		return cermin.Thought{}, false, fmt.Errorf("reading thought of message %d: %w", messageID, err)
	}
	return t, true, nil
}

// DeleteThought deletes the Thought id, with its Classifications, and the
// inbox updates that carry its text or its Acknowledgement's, all in one
// transaction. Those are the message that carried it, the edits of that
// message, and every reply to it or to its Acknowledgement, since a reply
// carries the text of the message it replies to.
func (db *DB) DeleteThought(ctx context.Context, id int64) error {
	if err := db.deleteThought(ctx, id); err != nil {
		return fmt.Errorf("deleting thought %d: %w", id, err)
	}
	return nil
}

func (db *DB) deleteThought(ctx context.Context, id int64) error {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit it only returns sql.ErrTxDone

	_, err = tx.ExecContext(ctx, `
		DELETE FROM telegram_updates
		WHERE EXISTS (
			SELECT 1 FROM thoughts AS t
			WHERE t.id = ?
				AND (message_id = t.tg_message_id OR reply_to_message_id IN (t.tg_message_id, t.ack_message_id))
		)`,
		id,
	)
	if err != nil {
		return err
	}
	// Its Classifications and their candidate Topics cascade.
	if _, err := tx.ExecContext(ctx, `DELETE FROM thoughts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// Thoughts returns every Thought, oldest first.
func (db *DB) Thoughts(ctx context.Context) ([]cermin.Thought, error) {
	return db.queryThoughts(ctx, `SELECT `+thoughtColumns+` FROM thoughts ORDER BY id`)
}

// OutOfDateAckThoughts returns the Thoughts from Telegram whose
// Acknowledgement is out of date, oldest first: those that have none, and
// those whose Acknowledgement shows a Classification other than their current
// one. An Acknowledgement that shows none is up to date until the Thought is
// classified.
func (db *DB) OutOfDateAckThoughts(ctx context.Context) ([]cermin.Thought, error) {
	// The join reads classifications once, where a subquery in WHERE would
	// read it once per Thought. IS NOT takes NULL as equal to NULL.
	return db.queryThoughts(ctx, `
		SELECT `+thoughtColumns+` FROM thoughts
		LEFT JOIN (
			SELECT thought_id, MAX(id) AS current_id FROM classifications GROUP BY thought_id
		) ON thought_id = thoughts.id
		WHERE tg_message_id IS NOT NULL
			AND (ack_message_id IS NULL OR ack_classification_id IS NOT current_id)
		ORDER BY thoughts.id`,
	)
}

// UnclassifiedThoughts returns up to limit Thoughts that have no
// Classification, oldest first.
func (db *DB) UnclassifiedThoughts(ctx context.Context, limit int) ([]cermin.Thought, error) {
	// NOT IN reads classifications once, where NOT EXISTS would read it once
	// per Thought.
	return db.queryThoughts(ctx, `
		SELECT `+thoughtColumns+` FROM thoughts
		WHERE id NOT IN (SELECT thought_id FROM classifications)
		ORDER BY id
		LIMIT ?`,
		limit,
	)
}

// queryThoughts runs query, which selects thoughtColumns, and returns the
// Thoughts it finds.
func (db *DB) queryThoughts(ctx context.Context, query string, args ...any) ([]cermin.Thought, error) {
	thoughts, err := queryAll(ctx, db.pool, scanThought, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing thoughts: %w", err)
	}
	return thoughts, nil
}

// thoughtColumns are the columns scanThought reads, in its order.
const thoughtColumns = `id, text, source, COALESCE(tg_message_id, 0), COALESCE(tg_reply_to_message_id, 0),
	COALESCE(ack_message_id, 0), COALESCE(ack_classification_id, 0), thought_at, received_at`

func scanThought(row scanner) (cermin.Thought, error) {
	var t cermin.Thought
	var thoughtAt, receivedAt string
	err := row.Scan(
		&t.ID,
		&t.Text,
		&t.Source,
		&t.TGMessageID,
		&t.TGReplyToMessageID,
		&t.AckMessageID,
		&t.AckClassificationID,
		&thoughtAt,
		&receivedAt,
	)
	if err != nil {
		return cermin.Thought{}, err
	}
	if t.ThoughtAt, err = time.Parse(timeLayout, thoughtAt); err != nil {
		return cermin.Thought{}, fmt.Errorf("thought %d: %w", t.ID, err)
	}
	if t.ReceivedAt, err = time.Parse(timeLayout, receivedAt); err != nil {
		return cermin.Thought{}, fmt.Errorf("thought %d: %w", t.ID, err)
	}
	return t, nil
}
