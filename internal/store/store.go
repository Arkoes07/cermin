// Package store keeps Cermin's data in one SQLite file: the Telegram inbox
// and the Thoughts captured from it.
package store

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

	_ "modernc.org/sqlite" // registers the "sqlite" driver; this package is its only user
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
	MessageID        int64  // 0 if the update carries no message
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

// Thought is a captured Thought.
type Thought struct {
	ID                 int64
	Text               string
	Source             string    // "telegram" or "whatsapp"
	TGMessageID        int64     // the Telegram message that carried it
	TGReplyToMessageID int64     // the message it replied to; 0 if none
	AckMessageID       int64     // the Acknowledgement's message; 0 until sent
	ThoughtAt          time.Time // Thought time
	ReceivedAt         time.Time // when Cermin stored it
	Language           string    // en, id or mixed; "" until classified
}

// SaveThought stores a Thought taken from a Telegram message and returns it
// as stored. If that message's Thought is already stored, it is returned
// unchanged, so saving the same message twice keeps one Thought.
func (db *DB) SaveThought(ctx context.Context, t Thought) (Thought, error) {
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
		return Thought{}, fmt.Errorf("saving thought from message %d: %w", t.TGMessageID, err)
	}
	row := db.pool.QueryRowContext(ctx,
		`SELECT `+thoughtColumns+` FROM thoughts WHERE tg_message_id = ?`,
		t.TGMessageID,
	)
	stored, err := scanThought(row)
	if err != nil {
		return Thought{}, fmt.Errorf("reading thought from message %d: %w", t.TGMessageID, err)
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

// Thoughts returns every Thought, oldest first.
func (db *DB) Thoughts(ctx context.Context) ([]Thought, error) {
	rows, err := db.pool.QueryContext(ctx, `SELECT `+thoughtColumns+` FROM thoughts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing thoughts: %w", err)
	}
	defer rows.Close()

	thoughts := []Thought{}
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

func scanThought(row interface{ Scan(dest ...any) error }) (Thought, error) {
	var t Thought
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
		return Thought{}, err
	}
	if t.ThoughtAt, err = time.Parse(timeLayout, thoughtAt); err != nil {
		return Thought{}, fmt.Errorf("thought %d: %w", t.ID, err)
	}
	if t.ReceivedAt, err = time.Parse(timeLayout, receivedAt); err != nil {
		return Thought{}, fmt.Errorf("thought %d: %w", t.ID, err)
	}
	return t, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}
