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
	// catchUpInterval is how often Run catches up.
	catchUpInterval = time.Minute
	// catchUpClassifyTimeout is how long the catch-up loop waits for a
	// Thought's Classification. No Acknowledgement waits on it, so a slow
	// classifier gets time to finish.
	catchUpClassifyTimeout = 60 * time.Second
	// catchUpLimit is the most Thoughts one catch-up classifies, to cap cost.
	catchUpLimit = 20
	// savedAck is the Acknowledgement of a Thought not yet classified.
	savedAck = "✓ saved"
	// topicsUsage is the reply to a /topics that can't be run.
	topicsUsage = "Reply to a Thought or its Acknowledgement with /topics and 1 to 3 Topics, separated by commas."
	// notReadyReply is the reply to a Correction of a Thought not yet
	// classified.
	notReadyReply = "Kind and Topics aren't ready yet. Try again in a minute."
	// deleteUsage is the reply to a /delete that can't be run.
	deleteUsage = "Reply to a Thought or its Acknowledgement with /delete."
	// deletedAck is the Acknowledgement of a deleted Thought.
	deletedAck = "🗑 deleted"
	// textOnlyReply is the reply to a message with no text, such as a photo.
	textOnlyReply = "Text only for now."
	// editReply is the reply to an edited message.
	editReply = "Edits aren't saved. Send it as a new Thought."
	// helpReply is the reply to /start and /help: the command list.
	helpReply = "Send any text to save it as a Thought, unless it starts with /.\n\n" +
		"Reply to a Thought or its Acknowledgement with:\n" +
		"/kind <kind> to correct its Kind\n" +
		"/topics <a>, <b>, <c> to replace its 1 to 3 Topics\n" +
		"/delete to delete it"
	// unknownCommandReply is the reply to an unknown command.
	unknownCommandReply = "Unknown command.\n\n" + helpReply
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
	offset   int64 // the next update_id to ask Telegram for

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
	return &Bot{db: db, tg: tg, ownerID: ownerID, classify: classify}
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

// catchUpEveryInterval calls CatchUp at once and then every catchUpInterval,
// until ctx is cancelled.
func (b *Bot) catchUpEveryInterval(ctx context.Context) {
	ticker := time.NewTicker(catchUpInterval)
	defer ticker.Stop()
	for {
		if err := b.CatchUp(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "catching up", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
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
	updates, err := b.db.UnprocessedUpdates(ctx)
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
		return b.replyAndMarkProcessed(
			ctx,
			in.ID,
			m,
			editReply,
		)
	case strings.HasPrefix(m.Text, "/"):
		return b.runCommand(ctx, in.ID, m)
	case m.Text == "":
		// ponytail: each photo of an album gets this reply, and so does a
		// service message such as your pin of a message; reply once per
		// media_group_id and skip service messages if it ever matters.
		return b.replyAndMarkProcessed(
			ctx,
			in.ID,
			m,
			textOnlyReply,
		)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	thought, err := b.db.SaveThought(ctx, cermin.Thought{
		Text:               m.Text,
		Source:             "telegram",
		TGMessageID:        m.ID,
		TGReplyToMessageID: m.ReplyToID(),
		ThoughtAt:          time.Unix(m.Date, 0),
		ReceivedAt:         time.Now(),
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
		slog.InfoContext(ctx, "thought captured",
			"thought_id", thought.ID,
			"update_id", in.ID,
		)
	}
	return b.syncAckAndMarkProcessed(ctx, thought.ID, in.ID)
}

// runCommand runs the owner's command, sent as message m in update updateID,
// and marks the update processed, or removes it for /delete. /start, /help and
// an unknown command get the command list.
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
		return b.correct(
			ctx,
			updateID,
			m,
			kindUsage(kinds),
			correctKind,
		)
	case "/topics":
		correctTopics := func(c cermin.Classification) (cermin.Classification, bool) {
			return c.CorrectTopics(strings.Split(args, ","))
		}
		return b.correct(
			ctx,
			updateID,
			m,
			topicsUsage,
			correctTopics,
		)
	case "/start", "/help":
		return b.replyAndMarkProcessed(
			ctx,
			updateID,
			m,
			helpReply,
		)
	default:
		return b.replyAndMarkProcessed(
			ctx,
			updateID,
			m,
			unknownCommandReply,
		)
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
	thought, isFound, err := b.db.ThoughtByMessage(ctx, m.ReplyToID())
	if err != nil {
		return err
	}
	if !isFound {
		return b.replyAndMarkProcessed(
			ctx,
			updateID,
			m,
			usage,
		)
	}
	current, isClassified, err := b.db.CurrentClassification(ctx, thought.ID)
	if err != nil {
		return err
	}
	if !isClassified {
		return b.replyAndMarkProcessed(
			ctx,
			updateID,
			m,
			notReadyReply,
		)
	}
	if current.TGMessageID == m.ID {
		// Processed again after a crash, the Correction is current already.
		return b.syncAckAndMarkProcessed(ctx, thought.ID, updateID)
	}

	correction, isValid := apply(current)
	if !isValid {
		return b.replyAndMarkProcessed(
			ctx,
			updateID,
			m,
			usage,
		)
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
// Thought, its Classifications and every inbox update that carries its text,
// including updateID, then edits the Acknowledgement to 🗑 deleted. A /delete
// it can't run gets a reply saying how, and is removed from the inbox too,
// since one that replies to a Thought deleted already still carries its text.
//
// ponytail: a later /kind, /topics or text reply to the deleted Thought's
// message, or an edit of it, brings its text back into the inbox, and stays;
// delete your message in Telegram after /delete to prevent that, or store
// replies without the replied-to copy if it ever matters.
func (b *Bot) deleteThought(ctx context.Context, updateID int64, m *telegram.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A message that isn't a reply has ReplyToID 0, which no Thought's messages
	// have.
	thought, isFound, err := b.db.ThoughtByMessage(ctx, m.ReplyToID())
	if err != nil {
		return err
	}
	if !isFound {
		if err := b.reply(ctx, m, deleteUsage); err != nil {
			return err
		}
		return b.db.DeleteUpdate(ctx, updateID)
	}
	if err := b.db.DeleteThought(ctx, thought.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought deleted", "thought_id", thought.ID, "update_id", updateID)

	// The Thought is gone, so the edit can't be retried: it is made even
	// during shutdown, and a failure is logged.
	//
	// ponytail: a failed edit, or a crash before it, leaves the
	// Acknowledgement showing the deleted Thought's Kind and Topics; record
	// the pending edit in the delete's transaction if that ever happens.
	err = b.tg.EditMessageText(
		context.WithoutCancel(ctx),
		b.ownerID,
		thought.AckMessageID,
		deletedAck,
	)
	if err != nil {
		slog.WarnContext(ctx, "showing deletion", "thought_id", thought.ID, "err", err)
	}
	return nil
}

// reply sends text in reply to the owner's message m.
func (b *Bot) reply(ctx context.Context, m *telegram.Message, text string) error {
	_, err := b.tg.SendMessage(
		ctx,
		b.ownerID,
		text,
		m.ID,
	)
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

// kindUsage is the reply to a /kind that can't be run, listing kinds.
func kindUsage(kinds []cermin.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name)
	}
	return "Reply to a Thought or its Acknowledgement with /kind and one of: " + strings.Join(names, ", ") + "."
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
	_, isClassified, err := b.db.CurrentClassification(ctx, t.ID)
	if err != nil || isClassified {
		return false, err
	}
	topics, err := b.db.TopicNames(ctx)
	if err != nil {
		return false, err
	}
	c, err := b.classify(
		ctx,
		t.Text,
		b.db.Kinds(),
		topics,
	)
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
	t, isFound, err := b.db.Thought(ctx, thoughtID)
	if err != nil || !isFound {
		return err
	}
	if t.TGMessageID == 0 {
		return nil // from WhatsApp, with no message to reply to
	}
	current, isClassified, err := b.db.CurrentClassification(ctx, thoughtID)
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
		if text, err = b.ackText(current); err != nil {
			// Logged and not returned, so that it never stalls the inbox.
			slog.WarnContext(ctx, "rendering acknowledgement",
				"thought_id", t.ID,
				"classification_id", current.ID,
				"err", err,
			)
			return nil
		}
	}

	ackID := t.AckMessageID
	if ackID == 0 {
		var ack telegram.Message
		ack, err = b.tg.SendMessage(
			ctx,
			b.ownerID,
			text,
			t.TGMessageID,
		)
		ackID = ack.ID
	} else {
		err = b.tg.EditMessageText(
			ctx,
			b.ownerID,
			ackID,
			text,
		)
	}
	// An Acknowledgement you deleted stays deleted: it counts as shown, so
	// its edit isn't retried and no message is sent to replace it.
	isDeleted := errors.Is(err, telegram.ErrMessageNotFound)
	if err != nil && !isDeleted {
		slog.WarnContext(ctx, "showing acknowledgement",
			"thought_id", t.ID,
			"classification_id", current.ID,
			"err", err,
		)
		return nil
	}
	// Once shown, it is recorded even during shutdown, or a restart would
	// show it again.
	err = b.db.SetAck(
		context.WithoutCancel(ctx),
		t.ID,
		ackID,
		current.ID,
	)
	if err != nil {
		return err
	}
	msg := "acknowledgement shown"
	if isDeleted {
		msg = "acknowledgement deleted, left as it is"
	}
	slog.InfoContext(ctx, msg,
		"thought_id", t.ID,
		"ack_message_id", ackID,
		"classification_id", current.ID,
	)
	return nil
}

// CatchUp finishes what a crash, the classifier or Telegram left undone. It
// brings every out-of-date Acknowledgement up to date, sending the ones never
// sent, then classifies up to catchUpLimit Thoughts that have no
// Classification, oldest first, and edits their Acknowledgements to show it.
// A Thought that fails is logged and left for the next call.
//
// ponytail: a Thought the classifier always fails on, such as one it
// refuses, costs a paid call on every call of CatchUp, and catchUpLimit of
// them would stall the rest; count attempts and give up on it if that ever
// happens.
func (b *Bot) CatchUp(ctx context.Context) error {
	outOfDate, err := b.db.OutOfDateAckThoughts(ctx)
	if err != nil {
		return err
	}
	// ponytail: unbounded; cap it at catchUpLimit if a Declared Topic
	// backfill hits Telegram's 429s.
	for _, t := range outOfDate {
		// Locked per Thought, so that this delays a Capture by one send or
		// edit at most. syncAck reads the Thought again under the lock, in
		// case a Capture or Correction has shown it since.
		b.mu.Lock()
		err := b.syncAck(ctx, t.ID)
		b.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "updating acknowledgement", "thought_id", t.ID, "err", err)
		}
	}

	// Under the lock no Capture is under way, so none of these Thoughts is
	// being classified by one.
	b.mu.Lock()
	unclassified, err := b.db.UnclassifiedThoughts(ctx, catchUpLimit)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	for _, t := range unclassified {
		if err := b.classifyLate(ctx, t); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "classifying thought late", "thought_id", t.ID, "err", err)
		}
	}
	return nil
}

// classifyLate gives a Thought that had no Classification when the catch-up
// listed it one, waiting up to catchUpClassifyTimeout for the classifier, and
// brings its Acknowledgement up to date to show it.
func (b *Bot) classifyLate(ctx context.Context, t cermin.Thought) error {
	// Unlocked, so that a slow classifier never holds up a Capture.
	isSaved, err := b.classifyThought(ctx, t, catchUpClassifyTimeout)
	if err != nil || !isSaved {
		// With none saved there is nothing new to show: the Thought is gone,
		// or whatever classified it shows that, or the next catch-up does.
		return err
	}
	slog.InfoContext(ctx, "thought classified late", "thought_id", t.ID)

	// Saved, it is shown even during shutdown.
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.syncAck(context.WithoutCancel(ctx), t.ID)
}

// ackText is the Acknowledgement of a Thought classified as c, such as
// "💡 idea · business, retail": the Kind and the kept Topics, best first,
// marked ✏️ if c is a Correction.
func (b *Bot) ackText(c cermin.Classification) (string, error) {
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
