package sqlite

import (
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// The vector is stored as little-endian float32s, which #27 reads back.
func TestDB_SaveEmbedding_KeepsVector(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)
	vector := []float32{0.1, -2.5, math.SmallestNonzeroFloat32, math.MaxFloat32}

	if err := db.SaveEmbedding(ctx, thought.ID, "model-test", vector); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	var blob []byte
	err := db.pool.QueryRowContext(ctx,
		`SELECT vector FROM embeddings WHERE thought_id = ? AND model = 'model-test'`,
		thought.ID,
	).Scan(&blob)
	if err != nil {
		t.Fatalf("reading vector: %v", err)
	}
	stored := make([]float32, len(blob)/4)
	if _, err := binary.Decode(blob, binary.LittleEndian, stored); err != nil {
		t.Fatalf("decoding vector: %v", err)
	}
	if !slices.Equal(stored, vector) {
		t.Errorf("stored %v, want %v", stored, vector)
	}
}

// The catch-up embeds unlocked, so a /delete can come first.
func TestDB_SaveEmbedding_SkipsDeletedThought(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)
	if err := db.DeleteThought(ctx, thought.ID, 0); err != nil {
		t.Fatalf("DeleteThought: %v", err)
	}

	if err := db.SaveEmbedding(ctx, thought.ID, "model-test", []float32{1, 2}); err != nil {
		t.Errorf("SaveEmbedding: %v, want nothing saved and no error", err)
	}
}

// Vectors are kept per model, so switching models embeds every Thought again.
func TestDB_GetThoughtsWithoutEmbeddingByModel_IgnoresOtherModels(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	thought := saveThought(t, db)
	if err := db.SaveEmbedding(ctx, thought.ID, "model-old", []float32{1, 2}); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	if got, err := db.GetThoughtsWithoutEmbeddingByModel(ctx, "model-old", 20); err != nil || len(got) != 0 {
		t.Errorf("GetThoughtsWithoutEmbeddingByModel(model-old) = %+v, %v; want none", got, err)
	}
	got, err := db.GetThoughtsWithoutEmbeddingByModel(ctx, "model-new", 20)
	if err != nil || len(got) != 1 || got[0].ID != thought.ID {
		t.Errorf("GetThoughtsWithoutEmbeddingByModel(model-new) = %+v, %v; want Thought %d", got, err, thought.ID)
	}
}

// Only the vectors from the model asked for come back, as SaveEmbedding
// stored them.
func TestDB_GetThoughtsWithEmbeddingByModel_ReadsVectorsFromModel(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openDB(t)
	first := saveThought(t, db)
	second, err := db.SaveThought(ctx, cermin.Thought{
		Text:        "Run on Sundays instead.",
		Source:      "telegram",
		TGMessageID: 8,
		ThoughtAt:   time.Now(),
		ReceivedAt:  time.Now(),
	})
	if err != nil {
		t.Fatalf("SaveThought: %v", err)
	}
	if _, err := db.SaveImportedThought(ctx, cermin.Thought{
		Text:       "Has no vector yet.",
		Source:     "whatsapp",
		ThoughtAt:  time.Now(),
		ReceivedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveImportedThought: %v", err)
	}
	vectors := map[int64][]float32{first.ID: {0.1, -2.5}, second.ID: {math.MaxFloat32, 0}}
	for id, vector := range vectors {
		if err := db.SaveEmbedding(ctx, id, "model-test", vector); err != nil {
			t.Fatalf("SaveEmbedding: %v", err)
		}
	}
	if err := db.SaveEmbedding(ctx, first.ID, "model-other", []float32{9, 9}); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	got, err := db.GetThoughtsWithEmbeddingByModel(ctx, "model-test")
	if err != nil {
		t.Fatalf("GetThoughtsWithEmbeddingByModel: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("GetThoughtsWithEmbeddingByModel = %+v, want Thoughts %d and %d", got, first.ID, second.ID)
	}
	for i, want := range []cermin.Thought{first, second} {
		if got[i].ID != want.ID || got[i].Text != want.Text || !slices.Equal(got[i].Vector, vectors[want.ID]) {
			t.Errorf("GetThoughtsWithEmbeddingByModel[%d] = %+v, want %+v with %v", i, got[i], want, vectors[want.ID])
		}
	}
}
