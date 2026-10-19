package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/workersai"
)

// writePairs writes to stdout, as CSV, each Thought that has an eligible
// earlier Thought, paired with its best one, best score first, with both
// Thought dates in WIB and an empty wanted column for you to label.
func writePairs(ctx context.Context) error {
	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	model := cmp.Or(os.Getenv("CERMIN_EMBED_MODEL"), workersai.DefaultModel)
	corpus, err := db.GetThoughtsWithEmbeddingByModel(ctx, model)
	if err := errors.Join(err, db.Close()); err != nil {
		return err
	}

	type pair struct {
		thought cermin.Thought
		best    cermin.ScoredThought
	}
	pairs := []pair{}
	// ponytail: ranks the whole Corpus for each Thought, O(n²); fine for a
	// one-off on a few thousand Thoughts.
	for _, t := range corpus {
		if ranked := cermin.RankEligible(t, corpus); len(ranked) > 0 {
			pairs = append(pairs, pair{thought: t.Thought, best: ranked[0]})
		}
	}
	slices.SortStableFunc(pairs, func(a, b pair) int {
		return cmp.Compare(b.best.Score, a.best.Score)
	})

	rows := [][]string{
		{"score", "thought_id", "past_id", "thought_date", "past_date", "thought_text", "past_text", "wanted"},
	}
	for _, p := range pairs {
		rows = append(rows, []string{
			strconv.FormatFloat(p.best.Score, 'f', 4, 64),
			strconv.FormatInt(p.thought.ID, 10),
			strconv.FormatInt(p.best.ID, 10),
			p.thought.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
			p.best.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
			p.thought.Text,
			p.best.Text,
			"",
		})
	}
	if err := csv.NewWriter(os.Stdout).WriteAll(rows); err != nil {
		return fmt.Errorf("writing pairs: %w", err)
	}
	// On stderr, so that an empty CSV, from a copy with no vectors or the
	// wrong model, says why.
	fmt.Fprintf(os.Stderr, "wrote %d pairs from %d Thoughts with a vector from %s\n", len(pairs), len(corpus), model)
	return nil
}
