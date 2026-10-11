-- Classification: the Kinds and Topics a Thought can have, and every
-- Classification it was given. Classifications are append-only (ADR-0008).

CREATE TABLE kinds (
    name       TEXT NOT NULL PRIMARY KEY,
    emoji      TEXT NOT NULL,
    definition TEXT NOT NULL, -- used in the classifier's prompt
    created_at TEXT NOT NULL,
    retired_at TEXT
);

-- The starting Kinds, from the PRD. Changing them takes a new migration and a
-- prompt-version bump.
INSERT INTO kinds (name, emoji, definition, created_at) VALUES
    ('idea',        '💡', 'a possibility I could act on or build, not yet committed',          strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('question',    '❓', 'something I''m asking, of myself or the world, with no answer yet', strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('observation', '👀', 'something I noticed or learned about the world or other people',   strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('reflection',  '🪞', 'looking inward: feelings, values, lessons about myself',            strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ('plan',        '📋', 'something I intend or decided to do',                               strftime('%Y-%m-%dT%H:%M:%fZ', 'now'));

CREATE TABLE topics (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL UNIQUE COLLATE NOCASE,
    origin      TEXT    NOT NULL CHECK (origin IN ('cermin', 'you', 'declared')),
    description TEXT,
    created_at  TEXT    NOT NULL,
    declared_at TEXT,
    archived_at TEXT
);

-- AUTOINCREMENT, so that a deleted Classification's ID never goes to a new one.
CREATE TABLE classifications (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    thought_id        INTEGER NOT NULL REFERENCES thoughts (id) ON DELETE CASCADE,
    source            TEXT    NOT NULL CHECK (source IN ('cermin', 'correction', 'declared')),
    kind              TEXT    NOT NULL REFERENCES kinds (name),
    kind_confidence   REAL,
    model             TEXT,
    prompt_version    TEXT,
    confidence_method TEXT,
    raw_output        TEXT,    -- includes kind_suggestion
    language          TEXT    CHECK (language IN ('en', 'id', 'mixed')), -- the Thought's, as the model detected it
    tg_message_id     INTEGER, -- the Correction's message
    created_at        TEXT    NOT NULL,
    -- Week 3 measures Cermin's Classifications, so each records what made it.
    CHECK (source <> 'cermin' OR (
        kind_confidence IS NOT NULL AND model IS NOT NULL AND prompt_version IS NOT NULL
        AND confidence_method IS NOT NULL AND raw_output IS NOT NULL AND language IS NOT NULL
    ))
);

-- Every candidate Topic of a Classification, in order, best first. Dropped
-- candidates are kept here by name only and never get a topics row.
CREATE TABLE classification_topics (
    classification_id INTEGER NOT NULL REFERENCES classifications (id) ON DELETE CASCADE,
    topic_name        TEXT    NOT NULL COLLATE NOCASE, -- matches topics.name ignoring case
    confidence        REAL,
    kept              INTEGER NOT NULL CHECK (kept IN (0, 1))
);
