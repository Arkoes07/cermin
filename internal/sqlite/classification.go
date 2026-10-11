package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

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

// GetTopicNames returns the names of the Topics that aren't archived, in name
// order.
func (db *DB) GetTopicNames(ctx context.Context) ([]string, error) {
	names, err := queryAll(ctx, db.pool, scanName, `SELECT name FROM topics WHERE archived_at IS NULL ORDER BY name`)
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
// Topic doesn't exist yet creates it, with c.Source's Topic origin. A
// candidate naming an existing Topic, in any case, takes that Topic's
// spelling. The confidences are stored as NULL if c.Source states none, and
// an empty language as NULL.
func (db *DB) SaveClassification(ctx context.Context, c cermin.Classification) (cermin.Classification, error) {
	stored, err := db.saveClassification(ctx, c)
	if err != nil {
		return cermin.Classification{}, fmt.Errorf("saving classification of thought %d: %w", c.ThoughtID, err)
	}
	return stored, nil
}

// SaveFirstClassification stores Cermin's Classification c as
// SaveClassification does, but only if its Thought exists and has no
// Classification from Cermin, and reports whether it did. So a Thought keeps
// the first Classification Cermin gives it, even when two processes classify
// it at once. A Correction needs a Classification first, so until declared
// Classifications arrive, a Thought with none from Cermin has none at all. It
// returns false and no error for a Thought deleted meanwhile.
func (db *DB) SaveFirstClassification(
	ctx context.Context,
	c cermin.Classification,
) (cermin.Classification, bool, error) {
	stored, err := db.saveClassification(ctx, c)
	if isNotInserted(err) {
		return cermin.Classification{}, false, nil
	}
	if err != nil {
		return cermin.Classification{}, false, fmt.Errorf(
			"saving first classification of thought %d: %w",
			c.ThoughtID,
			err,
		)
	}
	return stored, true, nil
}

// isNotInserted reports whether err, from saveClassification, means its
// Classification wasn't inserted: no row came back, since the Thought has
// Cermin's Classification, or the insert failed a foreign key, since the
// Thought is gone. No later step in saveClassification fails either way.
func isNotInserted(err error) bool {
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	sqliteErr, isSQLiteErr := errors.AsType[*sqlitedriver.Error](err)
	return isSQLiteErr && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// saveClassification stores c and its candidate Topics in one transaction.
func (db *DB) saveClassification(ctx context.Context, c cermin.Classification) (cermin.Classification, error) {
	tx, err := db.pool.BeginTx(ctx, nil)
	if err != nil {
		return cermin.Classification{}, err
	}
	defer tx.Rollback() //nolint:errcheck // after Commit it only returns sql.ErrTxDone

	c.CreatedAt = time.Now().UTC().Truncate(time.Millisecond) // as stored
	if c.ID, err = insertClassification(ctx, tx, c); err != nil {
		return cermin.Classification{}, err
	}

	if err := insertTopics(ctx, tx, c); err != nil {
		return cermin.Classification{}, err
	}
	if c.Topics, err = insertTopicCandidates(ctx, tx, c); err != nil {
		return cermin.Classification{}, err
	}

	if err := tx.Commit(); err != nil {
		return cermin.Classification{}, err
	}
	return c, nil
}

// insertClassification inserts c, without its candidate Topics, and returns
// its ID. It returns sql.ErrNoRows, and inserts nothing, for Cermin's
// Classification of a Thought that has one.
func insertClassification(ctx context.Context, tx *sql.Tx, c cermin.Classification) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		INSERT INTO classifications (thought_id, source, kind, kind_confidence, model, prompt_version,
			confidence_method, raw_output, language, tg_message_id, created_at)
		VALUES (:thought_id, :source, :kind, :kind_confidence, NULLIF(:model, ''), NULLIF(:prompt_version, ''),
			NULLIF(:confidence_method, ''), NULLIF(:raw_output, ''), NULLIF(:language, ''),
			NULLIF(:tg_message_id, 0), :created_at)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		sql.Named("thought_id", c.ThoughtID),
		sql.Named("source", c.Source),
		sql.Named("kind", c.Kind),
		sql.Named("kind_confidence", sql.Null[float64]{V: c.KindConfidence, Valid: c.Source.HasConfidence()}),
		sql.Named("model", c.Model),
		sql.Named("prompt_version", c.PromptVersion),
		sql.Named("confidence_method", c.ConfidenceMethod),
		sql.Named("raw_output", c.RawOutput),
		sql.Named("language", c.Language),
		sql.Named("tg_message_id", c.TGMessageID),
		sql.Named("created_at", formatTime(c.CreatedAt)),
	).Scan(&id)
	return id, err
}

// insertTopics creates the Topics that c's kept candidates name and that
// don't exist yet, with c.Source's Topic origin.
func insertTopics(ctx context.Context, tx *sql.Tx, c cermin.Classification) error {
	for _, t := range c.Topics {
		if !t.Kept {
			continue
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO topics (name, origin, created_at) VALUES (:name, :origin, :created_at)
			ON CONFLICT (name) DO NOTHING`,
			sql.Named("name", t.Name),
			sql.Named("origin", c.Source.TopicOrigin()),
			sql.Named("created_at", formatTime(c.CreatedAt)),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

// insertTopicCandidates stores c's candidate Topics as those of the
// Classification c.ID and returns them as stored. A candidate naming an
// existing Topic, in any case, takes that Topic's spelling.
func insertTopicCandidates(ctx context.Context, tx *sql.Tx, c cermin.Classification) ([]cermin.TopicCandidate, error) {
	topics := make([]cermin.TopicCandidate, 0, len(c.Topics))
	for _, t := range c.Topics {
		// ponytail: a kept name matching an archived Topic is assigned to it;
		// skip archived Topics here once /topic archive exists.
		err := tx.QueryRowContext(ctx, `
			INSERT INTO classification_topics (classification_id, topic_name, confidence, kept)
			VALUES (
				:classification_id, COALESCE((SELECT name FROM topics WHERE name = :name), :name), :confidence, :kept
			)
			RETURNING topic_name`,
			sql.Named("classification_id", c.ID),
			sql.Named("name", t.Name),
			sql.Named("confidence", sql.Null[float64]{V: t.Confidence, Valid: c.Source.HasConfidence()}),
			sql.Named("kept", t.Kept),
		).Scan(&t.Name)
		if err != nil {
			return nil, err
		}
		topics = append(topics, t)
	}
	return topics, nil
}

// GetClassificationsByThoughtID returns every Classification of the Thought
// thoughtID, oldest first, so the last one is current.
func (db *DB) GetClassificationsByThoughtID(ctx context.Context, thoughtID int64) ([]cermin.Classification, error) {
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
	for i, c := range classifications {
		if classifications[i].Topics, err = db.getTopicCandidatesByClassificationID(ctx, c.ID); err != nil {
			return nil, err
		}
	}
	return classifications, nil
}

// GetCurrentClassificationByThoughtID returns the current Classification of
// the Thought thoughtID, its latest, and false if it has none.
func (db *DB) GetCurrentClassificationByThoughtID(
	ctx context.Context,
	thoughtID int64,
) (cermin.Classification, bool, error) {
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
	if c.Topics, err = db.getTopicCandidatesByClassificationID(ctx, c.ID); err != nil {
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

// getTopicCandidatesByClassificationID returns the candidate Topics of the
// Classification classificationID, in the order they were stored.
func (db *DB) getTopicCandidatesByClassificationID(
	ctx context.Context,
	classificationID int64,
) ([]cermin.TopicCandidate, error) {
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
