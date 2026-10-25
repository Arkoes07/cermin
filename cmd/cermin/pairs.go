package main

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/workersai"
)

// writePairs writes to stdout, as CSV, every pair each Thought shortlists for
// a Resurfacing, for you to label. A pair labelled in the CSV at oldPath, if
// given, keeps its label.
func writePairs(ctx context.Context, oldPath string) error {
	labels := map[pairKey]string{}
	if oldPath != "" {
		f, err := os.Open(oldPath)
		if err != nil {
			return err
		}
		defer f.Close()
		if labels, err = readLabels(f); err != nil {
			return fmt.Errorf("reading %s: %w", oldPath, err)
		}
	}

	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	model := cmp.Or(os.Getenv("CERMIN_EMBED_MODEL"), workersai.DefaultModel)
	corpus, err := db.GetThoughtsWithEmbeddingByModel(ctx, model)
	if err := errors.Join(err, db.Close()); err != nil {
		return err
	}

	written, carried, err := writeShortlists(os.Stdout, corpus, labels)
	if err != nil {
		return err
	}
	// On stderr, so that an empty CSV, from a copy with no vectors or the
	// wrong model, says why.
	fmt.Fprintf(
		os.Stderr,
		"wrote %d pairs, %d with a carried label, from %d Thoughts with a vector from %s\n",
		written,
		carried,
		len(corpus),
		model,
	)
	return nil
}

// pairKey is a pair of a Thought and a past Thought, by their IDs as a CSV
// writes them.
type pairKey struct {
	thoughtID, pastID string
}

// readLabels reads the wanted label of each labelled pair in a CSV that
// cermin pairs wrote, in this format or #27's.
func readLabels(r io.Reader) (map[pairKey]string, error) {
	rows, err := readRows(r, "thought_id", "past_id", "wanted")
	if err != nil {
		return nil, err
	}
	labels := map[pairKey]string{}
	for _, row := range rows {
		if row["wanted"] != "" {
			labels[pairKey{thoughtID: row["thought_id"], pastID: row["past_id"]}] = row["wanted"]
		}
	}
	return labels, nil
}

// writeShortlists writes to w, as CSV, every pair each Thought in corpus
// shortlists, grouped by Thought, then by rank, with both Thought dates in
// WIB. A pair in labels keeps its label and has held_out n; any other has
// held_out y and an empty wanted column. It returns how many pairs it wrote
// and how many of them kept a label.
func writeShortlists(
	w io.Writer,
	corpus []cermin.EmbeddedThought,
	labels map[pairKey]string,
) (written, carried int, err error) {
	rows := [][]string{{
		"score", "rank", "thought_id", "past_id", "thought_date", "past_date", "thought_text", "past_text", "held_out",
		"wanted",
	}}

	// ponytail: shortlists the whole Corpus for each Thought, O(n²); fine for
	// a one-off on a few thousand Thoughts.
	for _, t := range corpus {
		for i, past := range cermin.Shortlist(t, corpus) {
			key := pairKey{thoughtID: strconv.FormatInt(t.ID, 10), pastID: strconv.FormatInt(past.ID, 10)}
			wanted, isCarried := labels[key]
			heldOut := "y"
			if isCarried {
				heldOut = "n"
				carried++
			}
			rows = append(rows, []string{
				strconv.FormatFloat(past.Score, 'f', 4, 64),
				strconv.Itoa(i + 1),
				key.thoughtID,
				key.pastID,
				t.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
				past.ThoughtAt.In(cermin.WIB).Format(time.DateOnly),
				t.Text,
				past.Text,
				heldOut,
				wanted,
			})
		}
	}

	if err := csv.NewWriter(w).WriteAll(rows); err != nil {
		return 0, 0, fmt.Errorf("writing pairs: %w", err)
	}
	return len(rows) - 1, carried, nil
}

// readRows reads a CSV with a header row from r and returns each later row as
// a map from column name to value. It fails if any of columns is missing.
func readRows(r io.Reader, columns ...string) ([]map[string]string, error) {
	records, err := csv.NewReader(r).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New("no header row")
	}
	header := records[0]
	// A spreadsheet may save the CSV with a byte order mark.
	header[0] = strings.TrimPrefix(header[0], "\ufeff")
	for _, c := range columns {
		if !slices.Contains(header, c) {
			return nil, fmt.Errorf("no %s column", c)
		}
	}

	rows := make([]map[string]string, 0, len(records)-1)
	for _, record := range records[1:] {
		row := make(map[string]string, len(header))
		for i, name := range header {
			row[name] = record[i]
		}
		rows = append(rows, row)
	}
	return rows, nil
}
