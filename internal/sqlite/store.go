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
	"slices"
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
	pool  *sql.DB
	kinds []cermin.Kind // read once in Open, since only a migration changes them
}

// Open opens the database at path, creating the file and its directory if
// needed, applies any migrations it hasn't had yet, and reads the Kinds.
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
	kinds, err := queryAll(ctx, pool, scanKind,
		`SELECT name, emoji, definition FROM kinds WHERE retired_at IS NULL ORDER BY rowid`)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listing kinds: %w", err), pool.Close())
	}
	return &DB{pool: pool, kinds: kinds}, nil
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

// InboxUpdate is a raw Telegram update in the inbox. Its message IDs let
// DeleteThought find the updates that carry a Thought's text.
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
	updates, err := queryAll(ctx, db.pool, scanUpdate, `
		SELECT update_id, payload, COALESCE(message_id, 0), COALESCE(reply_to_message_id, 0)
		FROM telegram_updates
		WHERE processed_at IS NULL
		ORDER BY update_id`)
	if err != nil {
		return nil, fmt.Errorf("listing unprocessed updates: %w", err)
	}
	return updates, nil
}

func scanUpdate(row scanner) (InboxUpdate, error) {
	var u InboxUpdate
	err := row.Scan(&u.ID, &u.Payload, &u.MessageID, &u.ReplyToMessageID)
	return u, err
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
	COALESCE(ack_message_id, 0), COALESCE(ack_classification_id, 0), thought_at, received_at,
	COALESCE(language, '')`

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

// Kinds returns the Kinds in use, in the order they were added, as they were
// when the database opened.
func (db *DB) Kinds() []cermin.Kind {
	return slices.Clone(db.kinds)
}

func scanKind(row scanner) (cermin.Kind, error) {
	var k cermin.Kind
	err := row.Scan(&k.Name, &k.Emoji, &k.Definition)
	return k, err
}

// TopicNames returns the names of the Topics that aren't archived, in name
// order.
func (db *DB) TopicNames(ctx context.Context) ([]string, error) {
	names, err := queryAll(ctx, db.pool, scanName,
		`SELECT name FROM topics WHERE archived_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	return names, nil
}

func scanName(row scanner) (string, error) {
	var name string
	err := row.Scan(&name)
	return name, err
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
	isCorrection := c.Source == cermin.SourceCorrection
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
	classifications, err := queryAll(ctx, db.pool, scanClassification, `
		SELECT `+classificationColumns+` FROM classifications
		WHERE thought_id = ?
		ORDER BY id`,
		thoughtID,
	)
	if err != nil {
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

// CurrentClassification returns the current Classification of the Thought
// thoughtID, its latest, and false if it has none.
func (db *DB) CurrentClassification(ctx context.Context, thoughtID int64) (cermin.Classification, bool, error) {
	row := db.pool.QueryRowContext(ctx, `
		SELECT `+classificationColumns+` FROM classifications
		WHERE thought_id = ?
		ORDER BY id DESC
		LIMIT 1`,
		thoughtID,
	)
	c, err := scanClassification(row)
	if errors.Is(err, sql.ErrNoRows) {
		return cermin.Classification{}, false, nil
	}
	if err != nil {
		return cermin.Classification{}, false, fmt.Errorf(
			"reading current classification of thought %d: %w",
			thoughtID,
			err,
		)
	}
	if c.Topics, err = db.topicCandidates(ctx, c.ID); err != nil {
		return cermin.Classification{}, false, err
	}
	return c, true, nil
}

// classificationColumns are the columns scanClassification reads, in its
// order.
const classificationColumns = `id, thought_id, source, kind, COALESCE(kind_confidence, 0), COALESCE(model, ''),
	COALESCE(prompt_version, ''), COALESCE(confidence_method, ''), COALESCE(raw_output, ''),
	COALESCE(tg_message_id, 0), created_at`

// scanClassification reads a Classification without its candidate Topics.
func scanClassification(row scanner) (cermin.Classification, error) {
	var c cermin.Classification
	var createdAt string
	err := row.Scan(
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
		return cermin.Classification{}, err
	}
	if c.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
		return cermin.Classification{}, fmt.Errorf("classification %d: %w", c.ID, err)
	}
	return c, nil
}

// topicCandidates returns the candidate Topics of a Classification, in the
// order they were stored.
func (db *DB) topicCandidates(ctx context.Context, classificationID int64) ([]cermin.TopicCandidate, error) {
	topics, err := queryAll(ctx, db.pool, scanTopicCandidate, `
		SELECT topic_name, COALESCE(confidence, 0), kept
		FROM classification_topics
		WHERE classification_id = ?
		ORDER BY rowid`,
		classificationID,
	)
	if err != nil {
		return nil, fmt.Errorf("listing topics of classification %d: %w", classificationID, err)
	}
	return topics, nil
}

func scanTopicCandidate(row scanner) (cermin.TopicCandidate, error) {
	var t cermin.TopicCandidate
	err := row.Scan(&t.Name, &t.Confidence, &t.Kept)
	return t, err
}

// scanner is a row to read: an *sql.Row, or the current row of an *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// queryAll runs query and returns every row it finds, read by scan, in an
// empty slice if there are none. It takes the pool rather than a *DB so that
// Open can use it.
func queryAll[T any](
	ctx context.Context,
	pool *sql.DB,
	scan func(scanner) (T, error),
	query string,
	args ...any,
) ([]T, error) {
	rows, err := pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	all := []T{}
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return all, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
