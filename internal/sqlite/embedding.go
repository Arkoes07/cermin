package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// SaveEmbedding stores vector as the vector of Thought thoughtID from model,
// as little-endian float32s. It saves nothing if the Thought has one from
// model already, or is gone.
func (db *DB) SaveEmbedding(ctx context.Context, thoughtID int64, model string, vector []float32) error {
	blob, err := binary.Append(nil, binary.LittleEndian, vector)
	if err != nil {
		return fmt.Errorf("encoding vector of thought %d: %w", thoughtID, err)
	}
	// The SELECT finds no row for a Thought deleted while it was embedded.
	_, err = db.pool.ExecContext(ctx, `
		INSERT INTO embeddings (thought_id, model, vector, created_at)
		SELECT id, :model, :vector, :created_at FROM thoughts WHERE id = :thought_id
		ON CONFLICT (thought_id, model) DO NOTHING`,
		sql.Named("model", model),
		sql.Named("vector", blob),
		sql.Named("created_at", formatTime(time.Now())),
		sql.Named("thought_id", thoughtID),
	)
	if err != nil {
		return fmt.Errorf("saving vector of thought %d: %w", thoughtID, err)
	}
	return nil
}

// GetThoughtsWithoutEmbeddingByModel returns up to limit Thoughts that have no
// vector from model, oldest first.
func (db *DB) GetThoughtsWithoutEmbeddingByModel(
	ctx context.Context,
	model string,
	limit int,
) ([]cermin.Thought, error) {
	// NOT IN reads embeddings once, where NOT EXISTS would read it once per
	// Thought.
	return db.queryThoughts(ctx, `
		SELECT `+thoughtColumns+` FROM thoughts
		WHERE id NOT IN (SELECT thought_id FROM embeddings WHERE model = :model)
		ORDER BY id
		LIMIT :limit`,
		sql.Named("model", model),
		sql.Named("limit", limit),
	)
}

// GetThoughtsWithEmbeddingByModel returns every Thought that has a vector from
// model, with that vector, oldest first.
func (db *DB) GetThoughtsWithEmbeddingByModel(ctx context.Context, model string) ([]cermin.EmbeddedThought, error) {
	thoughts, err := queryAll(ctx, db.pool, scanEmbeddedThought, `
		SELECT `+thoughtColumns+`, vector FROM thoughts
		JOIN embeddings ON thought_id = thoughts.id
		WHERE model = ?
		ORDER BY thoughts.id`,
		model,
	)
	if err != nil {
		return nil, fmt.Errorf("listing thoughts with a vector from %s: %w", model, err)
	}
	return thoughts, nil
}

// scanEmbeddedThought reads thoughtColumns, then a vector stored as
// SaveEmbedding stores it.
func scanEmbeddedThought(row scanner) (cermin.EmbeddedThought, error) {
	var blob []byte
	t, err := scanThought(rowWithTail{row: row, tail: []any{&blob}})
	if err != nil {
		return cermin.EmbeddedThought{}, err
	}
	vector := make([]float32, len(blob)/4)
	if _, err := binary.Decode(blob, binary.LittleEndian, vector); err != nil {
		return cermin.EmbeddedThought{}, fmt.Errorf("decoding vector of thought %d: %w", t.ID, err)
	}
	return cermin.EmbeddedThought{Thought: t, Vector: vector}, nil
}

// rowWithTail is a row whose Scan reads its last columns into tail, after the
// ones it is asked for, so that scanThought can read a row with more columns.
type rowWithTail struct {
	row  scanner
	tail []any
}

func (r rowWithTail) Scan(dest ...any) error {
	return r.row.Scan(append(dest, r.tail...)...)
}
