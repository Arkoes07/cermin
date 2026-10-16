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
