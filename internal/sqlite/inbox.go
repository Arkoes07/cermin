package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// InboxUpdate is a raw Telegram update in the inbox. Its message IDs let
// DeleteThought find the updates that carry a Thought's text.
type InboxUpdate struct {
	ID               int64  // Telegram's update_id
	Payload          []byte // the update's JSON, as Telegram sent it
	MessageID        int64  // the message, or edited message; 0 if the update carries neither
	ReplyToMessageID int64  // 0 if the message isn't a reply
}

// SaveUpdate puts an update in the inbox. An update already there is left as
// it is.
func (db *DB) SaveUpdate(ctx context.Context, u InboxUpdate) error {
	_, err := db.pool.ExecContext(ctx, `
		INSERT INTO telegram_updates (update_id, payload, message_id, reply_to_message_id, received_at)
		VALUES (:update_id, :payload, NULLIF(:message_id, 0), NULLIF(:reply_to_message_id, 0), :received_at)
		ON CONFLICT (update_id) DO NOTHING`,
		sql.Named("update_id", u.ID),
		sql.Named("payload", string(u.Payload)), // a string is stored as TEXT, a []byte as BLOB
		sql.Named("message_id", u.MessageID),
		sql.Named("reply_to_message_id", u.ReplyToMessageID),
		sql.Named("received_at", formatTime(time.Now())),
	)
	if err != nil {
		return fmt.Errorf("saving update %d: %w", u.ID, err)
	}
	return nil
}

// GetUnprocessedUpdates returns the inbox updates not yet marked processed,
// oldest first.
func (db *DB) GetUnprocessedUpdates(ctx context.Context) ([]InboxUpdate, error) {
	updates, err := queryAll(ctx, db.pool, scanUpdate, `
		SELECT update_id, payload, COALESCE(message_id, 0), COALESCE(reply_to_message_id, 0)
		FROM telegram_updates
		WHERE processed_at IS NULL
		ORDER BY update_id`)
	if err != nil {
		return nil, fmt.Errorf("listing unprocessed updates: %w", err)
	}
	return updates, nil
}

func scanUpdate(row scanner) (InboxUpdate, error) {
	var u InboxUpdate
	err := row.Scan(&u.ID, &u.Payload, &u.MessageID, &u.ReplyToMessageID)
	return u, err
}

// MarkProcessed records that an update has been fully handled.
func (db *DB) MarkProcessed(ctx context.Context, updateID int64) error {
	_, err := db.pool.ExecContext(ctx,
		`UPDATE telegram_updates SET processed_at = :processed_at WHERE update_id = :update_id`,
		sql.Named("processed_at", formatTime(time.Now())),
		sql.Named("update_id", updateID),
	)
	if err != nil {
		return fmt.Errorf("marking update %d processed: %w", updateID, err)
	}
	return nil
}

// DeleteUpdate removes an update from the inbox.
func (db *DB) DeleteUpdate(ctx context.Context, updateID int64) error {
	_, err := db.pool.ExecContext(ctx, `DELETE FROM telegram_updates WHERE update_id = ?`, updateID)
	if err != nil {
		return fmt.Errorf("deleting update %d: %w", updateID, err)
	}
	return nil
}
