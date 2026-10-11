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
		VALUES (:text, :source, :tg_message_id, NULLIF(:tg_reply_to_message_id, 0), :thought_at, :received_at)
		ON CONFLICT (tg_message_id) DO NOTHING`,
		sql.Named("text", t.Text),
		sql.Named("source", t.Source),
		sql.Named("tg_message_id", t.TGMessageID),
		sql.Named("tg_reply_to_message_id", t.TGReplyToMessageID),
		sql.Named("thought_at", formatTime(t.ThoughtAt)),
		sql.Named("received_at", formatTime(t.ReceivedAt)),
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
	_, err := db.pool.ExecContext(ctx, `
		UPDATE thoughts SET ack_message_id = :ack_message_id, ack_classification_id = NULLIF(:classification_id, 0)
		WHERE id = :thought_id`,
		sql.Named("ack_message_id", ackMessageID),
		sql.Named("classification_id", classificationID),
		sql.Named("thought_id", thoughtID),
	)
	if err != nil {
		return fmt.Errorf("setting acknowledgement of thought %d: %w", thoughtID, err)
	}
	return nil
}

// PostponeClassification records that the catch-up has failed to classify a
// Thought failures times, and is to try again once it is retryAt.
func (db *DB) PostponeClassification(ctx context.Context, thoughtID int64, failures int, retryAt time.Time) error {
	_, err := db.pool.ExecContext(ctx,
		`UPDATE thoughts SET classify_failures = :failures, classify_retry_at = :retry_at WHERE id = :thought_id`,
		sql.Named("failures", failures),
		sql.Named("retry_at", formatTime(retryAt)),
		sql.Named("thought_id", thoughtID),
	)
	if err != nil {
		return fmt.Errorf("postponing classification of thought %d: %w", thoughtID, err)
	}
	return nil
}

// GetThoughtByID returns the Thought id, and false if there is none.
func (db *DB) GetThoughtByID(ctx context.Context, id int64) (cermin.Thought, bool, error) {
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

// GetThoughtByMessageID returns the Thought that Telegram message messageID
// carried or acknowledged, and false if there is none.
func (db *DB) GetThoughtByMessageID(ctx context.Context, messageID int64) (cermin.Thought, bool, error) {
	row := db.pool.QueryRowContext(ctx,
		`SELECT `+thoughtColumns+` FROM thoughts WHERE tg_message_id = :message_id OR ack_message_id = :message_id`,
		sql.Named("message_id", messageID),
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

// GetThoughts returns every Thought, oldest first.
func (db *DB) GetThoughts(ctx context.Context) ([]cermin.Thought, error) {
	return db.queryThoughts(ctx, `SELECT `+thoughtColumns+` FROM thoughts ORDER BY id`)
}

// GetThoughtsWithOutOfDateAck returns the Thoughts from Telegram whose
// Acknowledgement is out of date, oldest first: those that have none, and
// those whose Acknowledgement shows a Classification other than their current
// one. An Acknowledgement that shows none is up to date until the Thought is
// classified.
func (db *DB) GetThoughtsWithOutOfDateAck(ctx context.Context) ([]cermin.Thought, error) {
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

// GetThoughtsDueForClassification returns up to limit Thoughts that have no
// Classification and are due at now, oldest first: those the catch-up has
// never failed to classify, and those it is to try again at now or earlier.
func (db *DB) GetThoughtsDueForClassification(ctx context.Context, now time.Time, limit int) ([]cermin.Thought, error) {
	// NOT IN reads classifications once, where NOT EXISTS would read it once
	// per Thought. Times are stored so that text order is time order.
	return db.queryThoughts(ctx, `
		SELECT `+thoughtColumns+` FROM thoughts
		WHERE id NOT IN (SELECT thought_id FROM classifications)
			AND (classify_retry_at IS NULL OR classify_retry_at <= :now)
		ORDER BY id
		LIMIT :limit`,
		sql.Named("now", formatTime(now)),
		sql.Named("limit", limit),
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
	COALESCE(ack_message_id, 0), COALESCE(ack_classification_id, 0), thought_at, received_at, classify_failures`

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
		&t.ClassifyFailures,
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
