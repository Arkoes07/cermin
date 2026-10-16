-- Embeddings: a vector of each Thought's raw text, per model (ADR-0010), so
-- that two models can sit side by side and switching models is a backfill.

CREATE TABLE embeddings (
    thought_id INTEGER NOT NULL REFERENCES thoughts (id) ON DELETE CASCADE,
    model      TEXT    NOT NULL,
    vector     BLOB    NOT NULL, -- little-endian float32s
    created_at TEXT    NOT NULL,
    PRIMARY KEY (thought_id, model)
);
