package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

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
// states none, and its language as NULL.
func (db *DB) SaveClassification(ctx context.Context, c cermin.Classification) (cermin.Classification, error) {
	stored, _, err := db.saveClassification(ctx, c, false)
	if err != nil {
		return cermin.Classification{}, fmt.Errorf("saving classification of thought %d: %w", c.ThoughtID, err)
	}
	return stored, nil
}

// SaveFirstClassification stores Cermin's Classification c as
// SaveClassification does, but only if its Thought exists and has no
// Classification, and reports whether it did. So a Thought keeps the first
// Classification Cermin gives it, even when two processes classify it at
// once. It returns false and no error for a Thought deleted meanwhile.
func (db *DB) SaveFirstClassification(
	ctx context.Context,
	c cermin.Classification,
) (cermin.Classification, bool, error) {
	stored, isSaved, err := db.saveClassification(ctx, c, true)
	if err != nil {
		return cermin.Classification{}, false, fmt.Errorf(
			"saving first classification of thought %d: %w",
			c.ThoughtID,
			err,
		)
	}
	return stored, isSaved, nil
}

// saveClassification stores c, or if isFirst only while its Thought exists
// and has no Classification, and reports whether it did.
func (db *DB) saveClassification(
	ctx context.Context,
	c cermin.Classification,
	isFirst bool,
) (cermin.Classification, bool, error) {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return cermin.Classification{}, false, err
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
	// isFirst's condition is checked in the INSERT itself, so that no other
	// process can write between the check and the write. Checked in a read
	// before it, another process's write would fail this transaction with
	// SQLITE_BUSY_SNAPSHOT, which busy_timeout doesn't retry. No row back
	// means not saved.
	err = tx.QueryRowContext(ctx, `
		INSERT INTO classifications (thought_id, source, kind, kind_confidence, model, prompt_version,
			confidence_method, raw_output, language, tg_message_id, created_at)
		SELECT ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), ?
		WHERE NOT ? OR (
			EXISTS (SELECT 1 FROM thoughts WHERE id = ?)
			AND NOT EXISTS (SELECT 1 FROM classifications WHERE thought_id = ?)
		)
		RETURNING id`,
		c.ThoughtID,
		c.Source,
		c.Kind,
		sql.Null[float64]{V: c.KindConfidence, Valid: !isCorrection},
		c.Model,
		c.PromptVersion,
		c.ConfidenceMethod,
		c.RawOutput,
		c.Language,
		c.TGMessageID,
		formatTime(c.CreatedAt),
		isFirst,
		c.ThoughtID,
		c.ThoughtID,
	).Scan(&c.ID)
	if isFirst && errors.Is(err, sql.ErrNoRows) {
		return cermin.Classification{}, false, nil
	}
	if err != nil {
		return cermin.Classification{}, false, err
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
				return cermin.Classification{}, false, err
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
			return cermin.Classification{}, false, err
		}
		topics = append(topics, t)
	}
	c.Topics = topics

	if err := tx.Commit(); err != nil {
		return cermin.Classification{}, false, err
	}
	return c, true, nil
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
	COALESCE(language, ''), COALESCE(tg_message_id, 0), created_at`

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
		&c.Language,
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
