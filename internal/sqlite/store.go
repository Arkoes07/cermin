// Package sqlite keeps Cermin's data in one SQLite file: the Telegram inbox,
// the Thoughts captured from it, their Classifications and their vectors.
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
	pool  *sql.DB
	kinds []cermin.Kind // read once in Open, since only a migration changes them
}

// Open opens the database at path, creating the file and its directory if
// needed, applies any migrations it hasn't had yet, and reads the Kinds.
func Open(ctx context.Context, path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating database directory: %w", err)
	}
	// The driver runs these pragmas on every new connection. secure_delete
	// zeroes deleted rows, so that backups made after a delete don't hold them.
	const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)" +
		"&_pragma=secure_delete(ON)"
	pool, err := sql.Open("sqlite", path+pragmas)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	// The poller, the processor and the catch-up each use one connection at a
	// time; week 3's MCP revisits the number. A fifth query waits for a free
	// connection, and never fails for lack of one.
	pool.SetMaxOpenConns(4)
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
