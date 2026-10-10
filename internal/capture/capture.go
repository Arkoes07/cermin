// Package capture turns the owner's Telegram messages into Thoughts,
// classifies them, and applies the owner's Corrections and deletes. Every
// update is stored in the inbox before anything else happens and processed
// from there, so a crash or a failed reply never loses one. A catch-up loop
// later finishes the Captures that a crash or the classifier left undone.
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

// ClassifyFunc gives a Thought's text a Classification, with one of kinds
// and candidate Topics that reuse the existing topics where one fits, and
// returns the Thought's language. (*claude.Classifier).Classify is one.
type ClassifyFunc func(
	ctx context.Context,
	text string,
	kinds []cermin.Kind,
	topics []string,
) (c cermin.Classification, language string, err error)

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
			if m.ReplyToMessage != nil {
				row.ReplyToMessageID = m.ReplyToMessage.ID
			}
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
		return b.replyAndMarkProcessed(ctx, in, editReply)
	case strings.HasPrefix(m.Text, "/"):
		return b.runCommand(ctx, in, m.Text)
	case m.Text == "":
		// ponytail: each photo of an album gets this reply, and so does a
		// service message such as your pin of a message; reply once per
		// media_group_id and skip service messages if it ever matters.
		return b.replyAndMarkProcessed(ctx, in, textOnlyReply)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	thought, err := b.db.SaveThought(ctx, cermin.Thought{
		Text:               m.Text,
		Source:             "telegram",
		TGMessageID:        in.MessageID,
		TGReplyToMessageID: in.ReplyToMessageID,
		ThoughtAt:          time.Unix(m.Date, 0),
		ReceivedAt:         time.Now(),
	})
	if err != nil {
		return err
	}
	// The Thought may already have its Acknowledgement if this update was
	// processed before a crash, or the catch-up sent it since. A crash between
	// sending and storing it still sends a second one.
	if thought.AckMessageID == 0 {
		if err := b.tg.SendChatAction(ctx, m.Chat.ID, "typing"); err != nil {
			slog.WarnContext(ctx, "showing typing", "thought_id", thought.ID, "err", err)
		}
		text, err := b.classifyThought(ctx, thought)
		if err != nil {
			// A slow or failing classifier never fails a Capture: the
			// Thought is saved and stays unclassified until the catch-up.
			slog.WarnContext(ctx, "classifying thought", "thought_id", thought.ID, "err", err)
			text = savedAck
		}
		ackID, err := b.acknowledge(ctx, thought, text)
		if err != nil {
			return err
		}
		slog.InfoContext(ctx, "thought captured",
			"thought_id", thought.ID,
			"update_id", in.ID,
			"ack_message_id", ackID,
		)
	}
	return b.db.MarkProcessed(ctx, in.ID)
}

// runCommand runs the owner's command text, sent as message in, and marks in
// processed, or removes it for /delete. /start, /help and an unknown command
// get the command list.
func (b *Bot) runCommand(ctx context.Context, in sqlite.InboxUpdate, text string) error {
	command, args, _ := strings.Cut(text, " ")
	switch command {
	case "/delete":
		return b.deleteThought(ctx, in)
	case "/kind", "/topics":
		if err := b.correct(ctx, in, command, args); err != nil {
			return err
		}
		return b.db.MarkProcessed(ctx, in.ID)
	case "/start", "/help":
		return b.replyAndMarkProcessed(ctx, in, helpReply)
	default:
		return b.replyAndMarkProcessed(ctx, in, unknownCommandReply)
	}
}

// correct runs /kind or /topics, sent as message in with args after the
// command, in reply to a Thought or its Acknowledgement. It adds the
// Correction that replaces the Thought's Kind or its whole set of Topics and
// keeps the rest of its current Classification, then edits the
// Acknowledgement to show it. A command it can't run gets a reply saying why.
func (b *Bot) correct(ctx context.Context, in sqlite.InboxUpdate, command, args string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	kinds, err := b.db.Kinds(ctx)
	if err != nil {
		return err
	}
	usage := topicsUsage
	if command == "/kind" {
		usage = kindUsage(kinds)
	}
	// A message that isn't a reply has ReplyToMessageID 0, which no Thought's
	// messages have.
	thought, isFound, err := b.db.ThoughtByMessage(ctx, in.ReplyToMessageID)
	if err != nil {
		return err
	}
	if !isFound {
		return b.reply(ctx, in, usage)
	}
	classifications, err := b.db.Classifications(ctx, thought.ID)
	if err != nil {
		return err
	}
	if len(classifications) == 0 {
		return b.reply(ctx, in, notReadyReply)
	}
	current := classifications[len(classifications)-1]
	if current.TGMessageID == in.MessageID {
		// Processed again after a crash, the Correction is current already.
		b.showCorrection(ctx, kinds, thought, current)
		return nil
	}

	var correction cermin.Classification
	var isValid bool
	if command == "/kind" {
		correction, isValid = current.CorrectKind(args, kinds)
	} else {
		correction, isValid = current.CorrectTopics(strings.Split(args, ","))
	}
	if !isValid {
		return b.reply(ctx, in, usage)
	}
	correction.TGMessageID = in.MessageID
	stored, err := b.db.SaveClassification(ctx, correction, "")
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought corrected",
		"thought_id", thought.ID,
		"classification_id", stored.ID,
		"update_id", in.ID,
	)
	b.showCorrection(ctx, kinds, thought, stored)
	return nil
}

// showCorrection edits the Acknowledgement of Thought t to show its
// Correction c. The Correction is saved by now, so a failure is logged and not
// returned: retrying an edit that always fails, such as of an Acknowledgement
// you deleted, would stall the inbox behind it.
//
// ponytail: an edit that fails only once, such as on a timeout, also leaves
// the old Classification showing until the next Correction; retry the edit
// if that ever happens.
func (b *Bot) showCorrection(ctx context.Context, kinds []cermin.Kind, t cermin.Thought, c cermin.Classification) {
	text, err := ackText(kinds, c)
	if err == nil {
		err = b.tg.EditMessageText(
			ctx,
			b.ownerID,
			t.AckMessageID,
			text,
		)
	}
	if err != nil {
		slog.WarnContext(ctx, "showing correction", "thought_id", t.ID, "classification_id", c.ID, "err", err)
	}
}

// deleteThought runs /delete, sent as message in, in reply to a Thought or its
// Acknowledgement. In one transaction it deletes the Thought, its
// Classifications and every inbox update that carries its text, including in
// itself, then edits the Acknowledgement to 🗑 deleted. A /delete it can't run
// gets a reply saying how, and is removed from the inbox too, since one that
// replies to a Thought deleted already still carries its text.
//
// ponytail: a later /kind, /topics or text reply to the deleted Thought's
// message, or an edit of it, brings its text back into the inbox, and stays;
// delete your message in Telegram after /delete to prevent that, or store
// replies without the replied-to copy if it ever matters.
func (b *Bot) deleteThought(ctx context.Context, in sqlite.InboxUpdate) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	// A message that isn't a reply has ReplyToMessageID 0, which no Thought's
	// messages have.
	thought, isFound, err := b.db.ThoughtByMessage(ctx, in.ReplyToMessageID)
	if err != nil {
		return err
	}
	if !isFound {
		if err := b.reply(ctx, in, deleteUsage); err != nil {
			return err
		}
		return b.db.DeleteUpdate(ctx, in.ID)
	}
	if err := b.db.DeleteThought(ctx, thought.ID); err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought deleted", "thought_id", thought.ID, "update_id", in.ID)

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

// reply sends text in reply to the owner's message in.
func (b *Bot) reply(ctx context.Context, in sqlite.InboxUpdate, text string) error {
	_, err := b.tg.SendMessage(
		ctx,
		b.ownerID,
		text,
		in.MessageID,
	)
	return err
}

// replyAndMarkProcessed sends text in reply to the owner's message in, and
// marks in processed.
func (b *Bot) replyAndMarkProcessed(ctx context.Context, in sqlite.InboxUpdate, text string) error {
	if err := b.reply(ctx, in, text); err != nil {
		return err
	}
	return b.db.MarkProcessed(ctx, in.ID)
}

// kindUsage is the reply to a /kind that can't be run, listing kinds.
func kindUsage(kinds []cermin.Kind) string {
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Name)
	}
	return "Reply to a Thought or its Acknowledgement with /kind and one of: " + strings.Join(names, ", ") + "."
}

// acknowledge sends text as a Thought's Acknowledgement, in reply to the
// Thought in the owner's private chat, records its message ID and returns it.
func (b *Bot) acknowledge(ctx context.Context, t cermin.Thought, text string) (int64, error) {
	ack, err := b.tg.SendMessage(
		ctx,
		b.ownerID,
		text,
		t.TGMessageID,
	)
	if err != nil {
		return 0, err
	}
	// Once sent, it is recorded even during shutdown, or a restart would send
	// it again.
	if err := b.db.SetAckMessageID(context.WithoutCancel(ctx), t.ID, ack.ID); err != nil {
		return 0, err
	}
	return ack.ID, nil
}

// classifyThought gives a Thought its Classification and returns the
// Acknowledgement that shows it. It gives up after classifyTimeout.
func (b *Bot) classifyThought(ctx context.Context, t cermin.Thought) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, classifyTimeout)
	defer cancel()

	kinds, err := b.db.Kinds(ctx)
	if err != nil {
		return "", err
	}
	// A Thought processed again, because its Acknowledgement failed to send,
	// keeps the Classification it already has.
	classifications, err := b.db.Classifications(ctx, t.ID)
	if err != nil {
		return "", err
	}
	if n := len(classifications); n > 0 {
		return ackText(kinds, classifications[n-1])
	}
	c, language, err := b.callClassifier(ctx, kinds, t)
	if err != nil {
		return "", err
	}
	stored, err := b.db.SaveClassification(ctx, c, language)
	if err != nil {
		return "", err
	}
	return ackText(kinds, stored)
}

// CatchUp finishes the Captures that a crash or the classifier left undone.
// It sends the Acknowledgement of each Thought that has none, then
// classifies up to catchUpLimit Thoughts that have no Classification, oldest
// first, and edits their ✓ saved Acknowledgements to show it. A Thought that
// fails is logged and left for the next call.
//
// ponytail: a Thought the classifier always fails on, such as one it
// refuses, costs a paid call on every call of CatchUp, and catchUpLimit of
// them would stall the rest; count attempts and give up on it if that ever
// happens.
func (b *Bot) CatchUp(ctx context.Context) error {
	kinds, err := b.db.Kinds(ctx)
	if err != nil {
		return err
	}
	if err := b.sendMissingAcks(ctx, kinds); err != nil {
		return err
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
		if err := b.classifyLate(ctx, kinds, t); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "classifying thought late", "thought_id", t.ID, "err", err)
		}
	}
	return nil
}

// sendMissingAcks sends the Acknowledgement of each Thought that has none, as
// a crash between saving a Thought and replying leaves it.
func (b *Bot) sendMissingAcks(ctx context.Context, kinds []cermin.Kind) error {
	// Under the lock no Capture is under way, so none of these Thoughts is
	// about to get its Acknowledgement from one.
	b.mu.Lock()
	defer b.mu.Unlock()
	unacked, err := b.db.UnackedThoughts(ctx)
	if err != nil {
		return err
	}
	for _, t := range unacked {
		if err := b.sendMissingAck(ctx, kinds, t); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "sending missing acknowledgement", "thought_id", t.ID, "err", err)
		}
	}
	return nil
}

// sendMissingAck sends the Acknowledgement of a Thought that has none,
// showing its Classification, or ✓ saved if it has none yet. The caller holds
// b.mu.
func (b *Bot) sendMissingAck(ctx context.Context, kinds []cermin.Kind, t cermin.Thought) error {
	classifications, err := b.db.Classifications(ctx, t.ID)
	if err != nil {
		return err
	}
	text := savedAck
	if n := len(classifications); n > 0 {
		if text, err = ackText(kinds, classifications[n-1]); err != nil {
			return err
		}
	}
	ackID, err := b.acknowledge(ctx, t, text)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "missing acknowledgement sent", "thought_id", t.ID, "ack_message_id", ackID)
	return nil
}

// classifyLate gives a Thought that has no Classification one, waiting up to
// catchUpClassifyTimeout for the classifier, and edits its ✓ saved
// Acknowledgement to show it.
func (b *Bot) classifyLate(ctx context.Context, kinds []cermin.Kind, t cermin.Thought) error {
	classifyCtx, cancel := context.WithTimeout(ctx, catchUpClassifyTimeout)
	defer cancel()
	c, language, err := b.callClassifier(classifyCtx, kinds, t)
	if err != nil {
		return err
	}
	// The paid call is done, so its Classification is saved and shown even
	// during shutdown.
	ctx = context.WithoutCancel(ctx)

	// Locked only now, so that a slow classifier never holds up a Capture.
	b.mu.Lock()
	defer b.mu.Unlock()
	// Meanwhile you may have deleted the Thought.
	current, isFound, err := b.db.Thought(ctx, t.ID)
	if err != nil || !isFound {
		return err
	}
	// Or the inbox may have classified the Thought, which keeps that
	// Classification, or sent its Acknowledgement.
	classifications, err := b.db.Classifications(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(classifications) > 0 {
		return nil
	}
	stored, err := b.db.SaveClassification(ctx, c, language)
	if err != nil {
		return err
	}
	slog.InfoContext(ctx, "thought classified late", "thought_id", t.ID, "classification_id", stored.ID)
	if current.AckMessageID == 0 {
		return nil // the Acknowledgement goes out showing the Classification
	}
	text, err := ackText(kinds, stored)
	if err != nil {
		return err
	}
	// ponytail: a failed edit, or a crash before it, leaves ✓ saved for good,
	// because the Thought is classified now; record which Classification the
	// Acknowledgement shows if that ever happens.
	return b.tg.EditMessageText(
		ctx,
		b.ownerID,
		current.AckMessageID,
		text,
	)
}

// callClassifier asks the classifier for a Thought's Classification, with
// the Topics it keeps marked, and returns it unsaved with the Thought's
// language.
func (b *Bot) callClassifier(
	ctx context.Context,
	kinds []cermin.Kind,
	t cermin.Thought,
) (c cermin.Classification, language string, err error) {
	topics, err := b.db.TopicNames(ctx)
	if err != nil {
		return cermin.Classification{}, "", err
	}
	c, language, err = b.classify(
		ctx,
		t.Text,
		kinds,
		topics,
	)
	if err != nil {
		return cermin.Classification{}, "", err
	}
	c.ThoughtID = t.ID
	c.Source = "cermin"
	c.Topics = cermin.KeepTopics(c.Topics)
	if len(c.Topics) == 0 {
		return cermin.Classification{}, "", errors.New("classified with no topic")
	}
	return c, language, nil
}

// ackText is the Acknowledgement of a Thought classified as c, such as
// "💡 idea · business, retail": the Kind and the kept Topics, best first,
// marked ✏️ if c is a Correction.
func ackText(kinds []cermin.Kind, c cermin.Classification) (string, error) {
	i := slices.IndexFunc(kinds, func(k cermin.Kind) bool { return k.Name == c.Kind })
	if i < 0 {
		return "", fmt.Errorf("classified with unknown kind %q", c.Kind)
	}
	text := kinds[i].Emoji + " " + c.Kind + " · " + strings.Join(c.KeptTopics(), ", ")
	if c.Source == "correction" {
		text = "✏️ " + text
	}
	return text, nil
}
