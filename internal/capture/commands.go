package capture

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	// topicsUsage is the reply to a /topics that can't be run.
	topicsUsage = "Reply to a Thought or its Acknowledgement with /topics and 1 to 3 Topics, separated by commas."
	// notReadyReply is the reply to a Correction of a Thought not yet
	// classified.
	notReadyReply = "Kind and Topics aren't ready yet. Try again in a minute."
	// deleteUsage is the reply to a /delete that can't be run.
	deleteUsage = "Reply to a Thought or its Acknowledgement with /delete."
	// deletedAck is the Acknowledgement of a deleted Thought.
	deletedAck = "🗑 deleted"
	// helpReply is the reply to /start and /help: the command list.
	helpReply = "Send any text to save it as a Thought, unless it starts with /.\n\n" +
		"Reply to a Thought or its Acknowledgement with:\n" +
		"/kind <kind> to correct its Kind\n" +
		"/topics <a>, <b>, <c> to replace its 1 to 3 Topics\n" +
		"/delete to delete it"
	// unknownCommandReply is the reply to an unknown command.
	unknownCommandReply = "Unknown command.\n\n" + helpReply
)

// runCommand runs the owner's command, sent as message m in update updateID,
// and marks the update processed, or removes it for a /delete that deletes a
// Thought. /start, /help and an unknown command get the command list.
func (b *Bot) runCommand(ctx context.Context, updateID int64, m *telegram.Message) error {
	command, args, _ := strings.Cut(m.Text, " ")
	switch command {
	case "/delete":
		return b.deleteThought(ctx, updateID, m)
	case "/kind":
		kinds := b.db.Kinds()
		correctKind := func(c cermin.Classification) (cermin.Classification, bool) {
			return c.CorrectKind(args, kinds)
		}
		return b.correct(ctx, updateID, m, formatKindUsage(kinds), correctKind)
	case "/topics":
		correctTopics := func(c cermin.Classification) (cermin.Classification, bool) {
			return c.CorrectTopics(strings.Split(args, ","))
		}
		return b.correct(ctx, updateID, m, topicsUsage, correctTopics)
	case "/start", "/help":
		return b.replyAndMarkProcessed(ctx, updateID, m, helpReply)
	default:
		return b.replyAndMarkProcessed(ctx, updateID, m, unknownCommandReply)
	}
}

// correct runs a Correction command, sent as message m in update updateID in
// reply to a Thought or its Acknowledgement, and marks the update processed.
// apply returns the Correction of the Thought's current Classification that
// the command asks for, which replaces its Kind or its whole set of Topics and
// keeps the rest, or false if the command's arguments ask for none. correct
// adds it and brings the Acknowledgement up to date. A command it can't run
// gets a reply saying why, which is usage unless the Thought isn't classified
// yet.
func (b *Bot) correct(
	ctx context.Context,
	updateID int64,
	m *telegram.Message,
	usage string,
	apply func(cermin.Classification) (cermin.Classification, bool),
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A message that isn't a reply has ReplyToID 0, which no Thought's messages
	// have.
	thought, isFound, err := b.db.GetThoughtByMessageID(ctx, m.ReplyToID())
	if err != nil {
		return err
	}
	if !isFound {
		return b.replyAndMarkProcessed(ctx, updateID, m, usage)
	}

	current, isClassified, err := b.db.GetCurrentClassificationByThoughtID(ctx, thought.ID)
	if err != nil {
		return err
	}
	if !isClassified {
		return b.replyAndMarkProcessed(ctx, updateID, m, notReadyReply)
	}

	if current.TGMessageID == m.ID {
		// Processed again after a crash, the Correction is current already.
		return b.syncAckAndMarkProcessed(ctx, thought.ID, updateID)
	}

	correction, isValid := apply(current)
	if !isValid {
		return b.replyAndMarkProcessed(ctx, updateID, m, usage)
	}
	correction.TGMessageID = m.ID
	stored, err := b.db.SaveClassification(ctx, correction)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought corrected",
		"thought_id", thought.ID,
		"classification_id", stored.ID,
		"update_id", updateID,
	)
	return b.syncAckAndMarkProcessed(ctx, thought.ID, updateID)
}

// deleteThought runs /delete, sent as message m in update updateID, in reply
// to a Thought or its Acknowledgement. In one transaction it deletes the
// Thought, its Classifications, the inbox updates that carry its message and
// updateID itself, then edits the Acknowledgement to 🗑 deleted. A /delete it
// can't run gets a reply saying how, and is marked processed.
func (b *Bot) deleteThought(ctx context.Context, updateID int64, m *telegram.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A message that isn't a reply has ReplyToID 0, which no Thought's messages
	// have.
	thought, isFound, err := b.db.GetThoughtByMessageID(ctx, m.ReplyToID())
	if err != nil {
		return err
	}
	if !isFound {
		return b.replyAndMarkProcessed(ctx, updateID, m, deleteUsage)
	}
	if err := b.db.DeleteThought(ctx, thought.ID, updateID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought deleted",
		"thought_id", thought.ID,
		"ack_message_id", thought.AckMessageID,
		"update_id", updateID,
	)

	if thought.AckMessageID != 0 {
		b.showDeletion(ctx, thought.AckMessageID)
	}
	return nil
}

// showDeletion edits the Acknowledgement ackMessageID of a deleted Thought to
// 🗑 deleted, then forgets it. One you deleted in Telegram is forgotten too. A
// failure is logged, and the catch-up retries it.
func (b *Bot) showDeletion(ctx context.Context, ackMessageID int64) {
	err := b.tg.EditMessageText(ctx, b.ownerID, ackMessageID, deletedAck)
	if err != nil && !errors.Is(err, telegram.ErrMessageNotFound) {
		slog.WarnContext(ctx, "showing deletion", "ack_message_id", ackMessageID, "err", err)
		return
	}
	if err := b.db.DeleteDeletedAck(ctx, ackMessageID); err != nil {
		slog.WarnContext(ctx, "forgetting shown deletion", "ack_message_id", ackMessageID, "err", err)
	}
}

// formatKindUsage returns the reply to a /kind that can't be run, listing
// kinds.
func formatKindUsage(kinds []cermin.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name)
	}
	return "Reply to a Thought or its Acknowledgement with /kind and one of: " + strings.Join(names, ", ") + "."
}
