-- Capture: the raw Telegram inbox and the Thoughts taken from it.
-- Times are UTC text, 2006-01-02T15:04:05.000Z.

CREATE TABLE telegram_updates (
    update_id           INTEGER PRIMARY KEY,
    payload             TEXT    NOT NULL, -- the update's raw JSON
    message_id          INTEGER,
    reply_to_message_id INTEGER,
    received_at         TEXT    NOT NULL,
    processed_at        TEXT
);

-- AUTOINCREMENT, so that a deleted Thought's ID never goes to a new one.
CREATE TABLE thoughts (
    id                     INTEGER PRIMARY KEY AUTOINCREMENT,
    text                   TEXT    NOT NULL,
    source                 TEXT    NOT NULL CHECK (source IN ('telegram', 'whatsapp')),
    tg_message_id          INTEGER UNIQUE,
    tg_reply_to_message_id INTEGER,
    ack_message_id         INTEGER UNIQUE,
    -- The Classification the Acknowledgement shows; NULL if it shows none.
    -- No foreign key: a Classification is only deleted with its Thought.
    ack_classification_id  INTEGER,
    thought_at             TEXT    NOT NULL,
    received_at            TEXT    NOT NULL,
    -- How many times the catch-up has failed to classify it, and when the
    -- catch-up next tries; NULL: at once.
    classify_failures      INTEGER NOT NULL DEFAULT 0,
    classify_retry_at      TEXT
);
