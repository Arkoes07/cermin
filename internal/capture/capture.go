// Package capture turns the owner's Telegram messages into Thoughts,
// classifies them, and applies the owner's Corrections and deletes. Every
// update is stored in the inbox before anything else happens and processed
// from there, so a crash or a failed reply never loses one. A catch-up loop
// later finishes what a crash, the classifier or Telegram left undone.
package capture

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	// pollTimeout is how long one getUpdates call waits for an update.
	pollTimeout = 50 * time.Second
	// retryDelay is the pause after a failed poll.
	retryDelay = 5 * time.Second
	// classifyTimeout is how long an Acknowledgement waits for the Thought's
	// Classification before it goes out without one.
	classifyTimeout = 8 * time.Second
	// savedAck is the Acknowledgement of a Thought not yet classified.
	savedAck = "✓ saved"
	// textOnlyReply is the reply to a message with no text, such as a photo.
	textOnlyReply = "Text only for now."
	// editReply is the reply to an edited message.
	editReply = "Edits aren't saved. Send it as a new Thought."
)

// ClassifyFunc gives a Thought's text a Classification: one of kinds,
// candidate Topics that reuse the existing topics where one fits, and the
// Thought's language. (*claude.Classifier).Classify is one.
type ClassifyFunc func(
	ctx context.Context,
	text string,
	kinds []cermin.Kind,
	topics []string,
) (cermin.Classification, error)

// Bot moves Telegram updates into the inbox, captures the owner's Thoughts
// and runs their Corrections and deletes from it, and catches up on the
// Captures left unfinished. One goroutine at a time may call Poll, or Run,
// which polls; the other methods are safe for concurrent use.
type Bot struct {
	db       *sqlite.DB
	tg       *telegram.Client
	ownerID  int64
	classify ClassifyFunc
	offset   int64            // the next update_id to ask Telegram for
	timeNow  func() time.Time // time.Now, unless a test sets another clock

	// mu is held while a Thought is captured, corrected, deleted or caught up
	// on, except while the catch-up waits for the classifier, so that two of
	// them never act on the same Thought at once.
	//
	// ponytail: one lock for every Thought, held across Telegram calls, so a
	// slow catch-up send or edit can delay a Capture by up to the send
	// timeout; lock per Thought if Captures ever wait on it.
	mu sync.Mutex
}

// New returns a Bot that captures the Thoughts of the Telegram user ownerID,
// ignoring everyone else, and classifies them with classify.
func New(db *sqlite.DB, tg *telegram.Client, ownerID int64, classify ClassifyFunc) *Bot {
	return &Bot{
		db:       db,
		tg:       tg,
		ownerID:  ownerID,
		classify: classify,
		timeNow:  time.Now,
	}
}

// Run processes the inbox and polls Telegram for more, and catches up at
// once and then every catchUpInterval in another goroutine, until ctx is
// cancelled. Failures are logged and retried.
func (b *Bot) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { b.catchUpEveryInterval(ctx) })

	for ctx.Err() == nil {
		// ponytail: polling waits while the inbox is processed; split them into
		// two goroutines if processing gets slow enough to delay the inbox.
		if err := b.ProcessInbox(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "processing inbox", "err", err)
		}
		if err := b.Poll(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "polling telegram", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(retryDelay):
			}
		}
	}
}

// Poll long-polls Telegram once and stores the updates it returns in the
// inbox.
func (b *Bot) Poll(ctx context.Context) error {
	updates, err := b.tg.GetUpdates(ctx, b.offset, pollTimeout)
	if err != nil {
		return err
	}
	for _, u := range updates {
		row := sqlite.InboxUpdate{ID: u.ID, Payload: u.Raw}
		// An edit carries the message's text too, so /delete must find it.
		if m := cmp.Or(u.Message, u.EditedMessage); m != nil {
			row.MessageID = m.ID
			row.ReplyToMessageID = m.ReplyToID()
		}
		if err := b.db.SaveUpdate(ctx, row); err != nil {
			return err
		}
		// A later offset tells Telegram to drop the update, so move it only
		// once the update is stored.
		b.offset = u.ID + 1
	}
	return nil
}

// ProcessInbox processes the unprocessed inbox updates in order. It stops at
// the first failure, leaving that update and the ones after it for the next
// call.
//
// ponytail: an update that always fails stalls the ones behind it; skip and
// alert on it if that ever happens.
func (b *Bot) ProcessInbox(ctx context.Context) error {
	updates, err := b.db.GetUnprocessedUpdates(ctx)
	if err != nil {
		return err
	}
	for _, in := range updates {
		if err := b.Process(ctx, in); err != nil {
			return fmt.Errorf("processing update %d: %w", in.ID, err)
		}
	}
	return nil
}

// Process handles one inbox update: the owner's text message becomes a
// Thought, which is classified and gets an Acknowledgement, the owner's /kind
// or /topics corrects one, and their /delete deletes one. The owner's edits,
// messages with no text and other commands get a reply saying what Cermin
// takes. Everyone else's updates are only marked processed.
func (b *Bot) Process(ctx context.Context, in sqlite.InboxUpdate) error {
	var update telegram.Update
	if err := json.Unmarshal(in.Payload, &update); err != nil {
		return fmt.Errorf("decoding: %w", err)
	}

	// Only the owner's messages in their private chat with the bot count: a
	// message ID from another chat could clash with a captured Thought's.
	// Their text is a Thought, unless it starts with /, which makes it a
	// command. An edit changes nothing, not even a command.
	m := cmp.Or(update.Message, update.EditedMessage)
	isOwner := m != nil && m.From != nil && m.From.ID == b.ownerID
	isPrivate := isOwner && m.Chat.ID == b.ownerID // a private chat's ID is the user's
	switch {
	case !isPrivate:
		slog.InfoContext(ctx, "update not captured", "update_id", in.ID)
		return b.db.MarkProcessed(ctx, in.ID)
	case update.EditedMessage != nil:
		return b.replyAndMarkProcessed(ctx, in.ID, m, editReply)
	case strings.HasPrefix(m.Text, "/"):
		return b.runCommand(ctx, in.ID, m)
	case m.Text == "":
		// ponytail: each photo of an album gets this reply, and so does a
		// service message such as your pin of a message; reply once per
		// media_group_id and skip service messages if it ever matters.
		return b.replyAndMarkProcessed(ctx, in.ID, m, textOnlyReply)
	default:
		return b.captureThought(ctx, in.ID, m)
	}
}

// captureThought captures the owner's text message m, sent in update
// updateID: it saves m as a Thought, classifies it and gives it an
// Acknowledgement, then marks the update processed.
func (b *Bot) captureThought(ctx context.Context, updateID int64, m *telegram.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	thought, err := b.db.SaveThought(ctx, cermin.Thought{
		Text:               m.Text,
		Source:             "telegram",
		TGMessageID:        m.ID,
		TGReplyToMessageID: m.ReplyToID(),
		ThoughtAt:          time.Unix(m.Date, 0),
		ReceivedAt:         b.timeNow(),
	})
	if err != nil {
		return err
	}

	// The Thought may already have its Acknowledgement if this update was
	// processed before a crash, or the catch-up sent it since; the catch-up
	// then classifies it if it isn't yet.
	if thought.AckMessageID == 0 {
		if err := b.tg.SendChatAction(ctx, m.Chat.ID, "typing"); err != nil {
			slog.WarnContext(ctx, "showing typing", "thought_id", thought.ID, "err", err)
		}
		if _, err := b.classifyThought(ctx, thought, classifyTimeout); err != nil {
			// A slow or failing classifier never fails a Capture: the
			// Thought is saved and stays unclassified until the catch-up.
			slog.WarnContext(ctx, "classifying thought", "thought_id", thought.ID, "err", err)
		}
		slog.InfoContext(ctx, "thought captured", "thought_id", thought.ID, "update_id", updateID)
	}
	return b.syncAckAndMarkProcessed(ctx, thought.ID, updateID)
}

// reply sends text in reply to the owner's message m.
func (b *Bot) reply(ctx context.Context, m *telegram.Message, text string) error {
	_, err := b.tg.SendMessage(ctx, b.ownerID, text, m.ID)
	return err
}

// replyAndMarkProcessed sends text in reply to the owner's message m, and
// marks update updateID, which carried it, processed.
func (b *Bot) replyAndMarkProcessed(ctx context.Context, updateID int64, m *telegram.Message, text string) error {
	if err := b.reply(ctx, m, text); err != nil {
		return err
	}
	return b.db.MarkProcessed(ctx, updateID)
}

// syncAckAndMarkProcessed brings the Acknowledgement of Thought thoughtID up
// to date, and marks update updateID, which captured or corrected it,
// processed. The caller holds b.mu.
func (b *Bot) syncAckAndMarkProcessed(ctx context.Context, thoughtID, updateID int64) error {
	if err := b.syncAck(ctx, thoughtID); err != nil {
		return err
	}
	return b.db.MarkProcessed(ctx, updateID)
}

// classifyThought gives a Thought its first Classification, waiting up to
// timeout for the classifier, and reports whether it saved one. It makes no
// paid call for a Thought that has a Classification, and saves none if the
// Thought got one, or was deleted, while the classifier ran.
func (b *Bot) classifyThought(ctx context.Context, t cermin.Thought, timeout time.Duration) (bool, error) {
	// The paid call is done once it returns, so its Classification is saved
	// even past the deadline or during shutdown.
	saveCtx := context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// A Thought processed again after a crash, before its Acknowledgement
	// was sent, keeps the Classification it has, and so does one the
	// catch-up reaches minutes after listing it.
	_, isClassified, err := b.db.GetCurrentClassificationByThoughtID(ctx, t.ID)
	if err != nil || isClassified {
		return false, err
	}

	topics, err := b.db.GetTopicNames(ctx)
	if err != nil {
		return false, err
	}
	c, err := b.classify(ctx, t.Text, b.db.Kinds(), topics)
	if err != nil {
		return false, err
	}

	c.ThoughtID = t.ID
	c.Source = cermin.SourceCermin
	c.Topics = cermin.KeepTopics(c.Topics)
	if len(c.Topics) == 0 {
		return false, errors.New("classified with no topic")
	}

	_, isSaved, err := b.db.SaveFirstClassification(saveCtx, c)
	if err != nil {
		return false, err
	}
	if !isSaved {
		// The paid call is wasted: meanwhile the Thought was deleted, or
		// classified, perhaps by another process.
		slog.InfoContext(ctx, "classification not saved", "thought_id", t.ID)
	}
	return isSaved, nil
}

// syncAck brings the Acknowledgement of Thought thoughtID up to date: it sends
// one if the Thought has none, or edits it if it shows anything other than
// the Thought's current Classification, and records what it shows. It does
// nothing if the Thought is gone, didn't come from Telegram, or has an
// Acknowledgement that is up to date. The caller holds b.mu.
//
// It shows the current Classification whatever its source. So a Declared
// Topic backfill's declared Classifications edit past Acknowledgements, too;
// to keep those as they are, skip declared Classifications here.
//
// A failed send or edit is logged and not returned, and the catch-up retries
// it; only SQLite errors are returned. A crash between sending and recording
// still sends a second one.
func (b *Bot) syncAck(ctx context.Context, thoughtID int64) error {
	t, isFound, err := b.db.GetThoughtByID(ctx, thoughtID)
	if err != nil || !isFound {
		return err
	}
	if t.TGMessageID == 0 {
		return nil // from WhatsApp, with no message to reply to
	}

	current, isClassified, err := b.db.GetCurrentClassificationByThoughtID(ctx, thoughtID)
	if err != nil {
		return err
	}
	// With no Classification, current.ID is 0, as is the
	// AckClassificationID of ✓ saved.
	if t.AckMessageID != 0 && t.AckClassificationID == current.ID {
		return nil
	}

	text := savedAck
	if isClassified {
		if text, err = b.renderAck(current); err != nil {
			// Logged and not returned, so that it never stalls the inbox.
			slog.WarnContext(ctx, "rendering acknowledgement",
				"thought_id", t.ID,
				"classification_id", current.ID,
				"err", err,
			)
			return nil
		}
	}

	ackID, isDeleted, err := b.showAck(ctx, t, text)
	if err != nil {
		slog.WarnContext(ctx, "showing acknowledgement",
			"thought_id", t.ID,
			"classification_id", current.ID,
			"err", err,
		)
		return nil
	}

	// Once shown, it is recorded even during shutdown, or a restart would
	// show it again.
	if err := b.db.SetAck(context.WithoutCancel(ctx), t.ID, ackID, current.ID); err != nil {
		return err
	}
	msg := "acknowledgement shown"
	if isDeleted {
		msg = "acknowledgement deleted, left as it is"
	}
	slog.InfoContext(ctx, msg, "thought_id", t.ID, "ack_message_id", ackID, "classification_id", current.ID)
	return nil
}

// showAck shows text as the Acknowledgement of Thought t: it sends it in
// reply to t's message if t has no Acknowledgement, or edits t's
// Acknowledgement to read it. It returns the Acknowledgement's message ID.
// An Acknowledgement you deleted stays deleted: it counts as shown, so its
// edit isn't retried and no message is sent to replace it, and isDeleted
// reports it.
func (b *Bot) showAck(ctx context.Context, t cermin.Thought, text string) (ackID int64, isDeleted bool, err error) {
	if t.AckMessageID == 0 {
		var ack telegram.Message
		ack, err = b.tg.SendMessage(ctx, b.ownerID, text, t.TGMessageID)
		return ack.ID, false, err
	}
	err = b.tg.EditMessageText(ctx, b.ownerID, t.AckMessageID, text)
	if errors.Is(err, telegram.ErrMessageNotFound) {
		return t.AckMessageID, true, nil
	}
	return t.AckMessageID, false, err
}

// renderAck returns the Acknowledgement of a Thought classified as c, such as
// "💡 idea · business, retail": the Kind and the kept Topics, best first,
// marked ✏️ if c is a Correction.
func (b *Bot) renderAck(c cermin.Classification) (string, error) {
	kinds := b.db.Kinds()
	i := slices.IndexFunc(kinds, func(k cermin.Kind) bool { return k.Name == c.Kind })
	if i < 0 {
		return "", fmt.Errorf("classified with unknown kind %q", c.Kind)
	}
	text := kinds[i].Emoji + " " + c.Kind + " · " + strings.Join(c.KeptTopics(), ", ")
	if c.Source == cermin.SourceCorrection {
		text = "✏️ " + text
	}
	return text, nil
}
