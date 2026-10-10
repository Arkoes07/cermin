// Package sqlite keeps Cermin's data in one SQLite file: the Telegram inbox,
// the Thoughts captured from it and their Classifications.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; outside tests, this package is its only user

	"github.com/arkoes07/cermin/internal/cermin"
)

//go:embed migrations/*.sql
var migrations embed.FS

// timeLayout is how times are stored: UTC, to the millisecond, fixed width so
// that text order is time order.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// DB is Cermin's database.
type DB struct {
	pool *sql.DB
}

// Open opens the database at path, creating the file and its directory if
// needed, and applies any migrations it hasn't had yet.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	// The driver runs these pragmas on every new connection.
	const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	// ponytail: the connection pool is left unbounded because one goroutine
	// uses it; bound it with SetMaxOpenConns once MCP readers share it.
	pool, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		return nil, errors.Join(err, pool.Close())
	}
	return &DB{pool: pool}, nil
}

// Close closes the database.
func (db *DB) Close() error {
	return db.pool.Close()
}

// migrate applies the embedded migrations in file name order. PRAGMA
// user_version counts those already applied.
func migrate(ctx context.Context, pool *sql.DB) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("listing migrations: %w", err)
	}
	var version int
	if err := pool.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if version > len(names) {
		return fmt.Errorf("database schema version %d is newer than this binary's %d", version, len(names))
	}
	for i, name := range names[version:] {
		if err := applyMigration(ctx, pool, name, version+i+1); err != nil {
			return fmt.Errorf("applying %s: %w", name, err)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, pool *sql.DB, name string, version int) error {
	script, err := migrations.ReadFile(name)
	if err != nil {
		return err
	}
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit it only returns sql.ErrTxDone
	if _, err := tx.ExecContext(ctx, string(script)); err != nil {
		return err
	}
	// PRAGMA takes no bound parameters; version is a number we computed.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}

// InboxUpdate is a raw Telegram update in the inbox.
type InboxUpdate struct {
	ID               int64  // Telegram's update_id
	Payload          []byte // the update's JSON, as Telegram sent it
	MessageID        int64  // the message, or edited message; 0 if the update carries neither
	ReplyToMessageID int64  // 0 if the message isn't a reply
}

// SaveUpdate puts an update in the inbox. An update already there is left as
// it is.
func (db *DB) SaveUpdate(ctx context.Context, u InboxUpdate) error {
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO telegram_updates (update_id, payload, message_id, reply_to_message_id, received_at)
		VALUES (?, ?, NULLIF(?, 0), NULLIF(?, 0), ?)
		ON CONFLICT (update_id) DO NOTHING`,
		u.ID,
		string(u.Payload), // a string is stored as TEXT, a []byte as BLOB
		u.MessageID,
		u.ReplyToMessageID,
		formatTime(time.Now()),
	)
	if err != nil {
		return fmt.Errorf("saving update %d: %w", u.ID, err)
	}
	return nil
}

// UnprocessedUpdates returns the inbox updates not yet marked processed,
// oldest first.
func (db *DB) UnprocessedUpdates(ctx context.Context) ([]InboxUpdate, error) {
	rows, err := db.pool.QueryContext(ctx, `
		SELECT update_id, payload, COALESCE(message_id, 0), COALESCE(reply_to_message_id, 0)
		FROM telegram_updates
		WHERE processed_at IS NULL
		ORDER BY update_id`)
	if err != nil {
		return nil, fmt.Errorf("listing unprocessed updates: %w", err)
	}
	defer rows.Close()

	updates := []InboxUpdate{}
	for rows.Next() {
		var u InboxUpdate
		if err := rows.Scan(&u.ID, &u.Payload, &u.MessageID, &u.ReplyToMessageID); err != nil {
			return nil, fmt.Errorf("reading unprocessed update: %w", err)
		}
		updates = append(updates, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing unprocessed updates: %w", err)
	}
	return updates, nil
}

// MarkProcessed records that an update has been fully handled.
func (db *DB) MarkProcessed(ctx context.Context, updateID int64) error {
	_, err := db.pool.ExecContext(ctx,
		`UPDATE telegram_updates SET processed_at = ? WHERE update_id = ?`,
		formatTime(time.Now()),
		updateID,
	)
	if err != nil {
		return fmt.Errorf("marking update %d processed: %w", updateID, err)
	}
	return nil
}

// DeleteUpdate removes an update from the inbox.
func (db *DB) DeleteUpdate(ctx context.Context, updateID int64) error {
	_, err := db.pool.ExecContext(ctx, `DELETE FROM telegram_updates WHERE update_id = ?`, updateID)
	if err != nil {
		return fmt.Errorf("deleting update %d: %w", updateID, err)
	}
	return nil
}

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

// SetAckMessageID records the message ID of a Thought's Acknowledgement.
func (db *DB) SetAckMessageID(ctx context.Context, thoughtID, ackMessageID int64) error {
	_, err := db.pool.ExecContext(ctx,
		`UPDATE thoughts SET ack_message_id = ? WHERE id = ?`,
		ackMessageID,
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

// UnackedThoughts returns the Thoughts from Telegram that have no
// Acknowledgement, oldest first.
func (db *DB) UnackedThoughts(ctx context.Context) ([]cermin.Thought, error) {
	return db.queryThoughts(ctx, `
		SELECT `+thoughtColumns+` FROM thoughts
		WHERE ack_message_id IS NULL AND tg_message_id IS NOT NULL
		ORDER BY id`,
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
	rows, err := db.pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing thoughts: %w", err)
	}
	defer rows.Close()

	thoughts := []cermin.Thought{}
	for rows.Next() {
		t, err := scanThought(rows)
		if err != nil {
			return nil, fmt.Errorf("reading thought: %w", err)
		}
		thoughts = append(thoughts, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing thoughts: %w", err)
	}
	return thoughts, nil
}

// thoughtColumns are the columns scanThought reads, in its order.
const thoughtColumns = `id, text, source, COALESCE(tg_message_id, 0), COALESCE(tg_reply_to_message_id, 0),
	COALESCE(ack_message_id, 0), thought_at, received_at, COALESCE(language, '')`

func scanThought(row interface{ Scan(dest ...any) error }) (cermin.Thought, error) {
	var t cermin.Thought
	var thoughtAt, receivedAt string
	err := row.Scan(
		&t.ID,
		&t.Text,
		&t.Source,
		&t.TGMessageID,
		&t.TGReplyToMessageID,
		&t.AckMessageID,
		&thoughtAt,
		&receivedAt,
		&t.Language,
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

// Kinds returns the Kinds in use, in the order they were added.
func (db *DB) Kinds(ctx context.Context) ([]cermin.Kind, error) {
	rows, err := db.pool.QueryContext(ctx,
		`SELECT name, emoji, definition FROM kinds WHERE retired_at IS NULL ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("listing kinds: %w", err)
	}
	defer rows.Close()

	kinds := []cermin.Kind{}
	for rows.Next() {
		var k cermin.Kind
		if err := rows.Scan(&k.Name, &k.Emoji, &k.Definition); err != nil {
			return nil, fmt.Errorf("reading kind: %w", err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing kinds: %w", err)
	}
	return kinds, nil
}

// TopicNames returns the names of the Topics that aren't archived, in name
// order.
func (db *DB) TopicNames(ctx context.Context) ([]string, error) {
	rows, err := db.pool.QueryContext(ctx,
		`SELECT name FROM topics WHERE archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	defer rows.Close()

	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("reading topic: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	return names, nil
}

// SaveClassification stores c and returns it as stored. A kept candidate whose
// Topic doesn't exist yet creates it, with origin cermin, or you for a
// Correction. A candidate naming an existing Topic, in any case, takes that
// Topic's spelling. A Correction's confidences are stored as NULL, since it
// states none. A language other than "" becomes the Thought's.
func (db *DB) SaveClassification(ctx context.Context, c cermin.Classification, language string) (cermin.Classification, error) {
	stored, err := db.saveClassification(ctx, c, language)
	if err != nil {
		return cermin.Classification{}, fmt.Errorf("saving classification of thought %d: %w", c.ThoughtID, err)
	}
	return stored, nil
}

func (db *DB) saveClassification(ctx context.Context, c cermin.Classification, language string) (cermin.Classification, error) {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return cermin.Classification{}, err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit it only returns sql.ErrTxDone

	// A Correction is yours: it states no confidence, and a Topic it names
	// first is your Topic.
	isCorrection := c.Source == "correction"
	origin := "cermin"
	if isCorrection {
		origin = "you"
	}
	c.CreatedAt = time.Now().UTC().Truncate(time.Millisecond) // as stored
	err = tx.QueryRowContext(ctx, `
		INSERT INTO classifications (thought_id, source, kind, kind_confidence, model, prompt_version,
			confidence_method, raw_output, tg_message_id, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), ?)
		RETURNING id`,
		c.ThoughtID,
		c.Source,
		c.Kind,
		sql.Null[float64]{V: c.KindConfidence, Valid: !isCorrection},
		c.Model,
		c.PromptVersion,
		c.ConfidenceMethod,
		c.RawOutput,
		c.TGMessageID,
		formatTime(c.CreatedAt),
	).Scan(&c.ID)
	if err != nil {
		return cermin.Classification{}, err
	}

	topics := make([]cermin.TopicCandidate, 0, len(c.Topics))
	for _, t := range c.Topics {
		// ponytail: a kept name matching an archived Topic is assigned to it;
		// skip archived Topics here once /topic archive exists.
		if t.Kept {
			_, err := tx.ExecContext(ctx, `
				INSERT INTO topics (name, origin, created_at) VALUES (?, ?, ?)
				ON CONFLICT (name) DO NOTHING`,
				t.Name,
				origin,
				formatTime(c.CreatedAt),
			)
			if err != nil {
				return cermin.Classification{}, err
			}
		}
		err := tx.QueryRowContext(ctx, `
			INSERT INTO classification_topics (classification_id, topic_name, confidence, kept)
			VALUES (?, COALESCE((SELECT name FROM topics WHERE name = ?), ?), ?, ?)
			RETURNING topic_name`,
			c.ID,
			t.Name,
			t.Name,
			sql.Null[float64]{V: t.Confidence, Valid: !isCorrection},
			t.Kept,
		).Scan(&t.Name)
		if err != nil {
			return cermin.Classification{}, err
		}
		topics = append(topics, t)
	}
	c.Topics = topics

	if language != "" {
		_, err := tx.ExecContext(ctx,
			`UPDATE thoughts SET language = ? WHERE id = ?`,
			language,
			c.ThoughtID,
		)
		if err != nil {
			return cermin.Classification{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return cermin.Classification{}, err
	}
	return c, nil
}

// Classifications returns every Classification of the Thought thoughtID,
// oldest first, so the last one is current.
func (db *DB) Classifications(ctx context.Context, thoughtID int64) ([]cermin.Classification, error) {
	rows, err := db.pool.QueryContext(ctx, `
		SELECT id, thought_id, source, kind, COALESCE(kind_confidence, 0), COALESCE(model, ''),
			COALESCE(prompt_version, ''), COALESCE(confidence_method, ''), COALESCE(raw_output, ''),
			COALESCE(tg_message_id, 0), created_at
		FROM classifications
		WHERE thought_id = ?
		ORDER BY id`,
		thoughtID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing classifications of thought %d: %w", thoughtID, err)
	}
	defer rows.Close()

	classifications := []cermin.Classification{}
	for rows.Next() {
		var c cermin.Classification
		var createdAt string
		err := rows.Scan(
			&c.ID,
			&c.ThoughtID,
			&c.Source,
			&c.Kind,
			&c.KindConfidence,
			&c.Model,
			&c.PromptVersion,
			&c.ConfidenceMethod,
			&c.RawOutput,
			&c.TGMessageID,
			&createdAt,
		)
		if err != nil {
			return nil, fmt.Errorf("reading classification of thought %d: %w", thoughtID, err)
		}
		if c.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
			return nil, fmt.Errorf("classification %d: %w", c.ID, err)
		}
		classifications = append(classifications, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing classifications of thought %d: %w", thoughtID, err)
	}

	// ponytail: one query per Classification, fine for the few a Thought has;
	// join them if a caller ever lists many Thoughts' Classifications.
	for i := range classifications {
		if classifications[i].Topics, err = db.topicCandidates(ctx, classifications[i].ID); err != nil {
			return nil, err
		}
	}
	return classifications, nil
}

// topicCandidates returns the candidate Topics of a Classification, in the
// order they were stored.
func (db *DB) topicCandidates(ctx context.Context, classificationID int64) ([]cermin.TopicCandidate, error) {
	rows, err := db.pool.QueryContext(ctx, `
		SELECT topic_name, COALESCE(confidence, 0), kept
		FROM classification_topics
		WHERE classification_id = ?
		ORDER BY rowid`,
		classificationID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing topics of classification %d: %w", classificationID, err)
	}
	defer rows.Close()

	topics := []cermin.TopicCandidate{}
	for rows.Next() {
		var t cermin.TopicCandidate
		if err := rows.Scan(&t.Name, &t.Confidence, &t.Kept); err != nil {
			return nil, fmt.Errorf("reading topic of classification %d: %w", classificationID, err)
		}
		topics = append(topics, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing topics of classification %d: %w", classificationID, err)
	}
	return topics, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
