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
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/sqlite"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	// pollTimeout is how long one getUpdates call waits for an update.
	pollTimeout = 50 * time.Second
	// staleAfter is how long after the last successful getUpdates the Bot
	// stops counting as healthy.
	staleAfter = 2 * time.Minute
	// retryDelay is the pause after a failed poll, or a failure to process
	// the inbox.
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
	// maxAttempts is how many times processing an update may begin before
	// it is skipped.
	maxAttempts = 5
	// skippedReply tells the owner of an update skipped after maxAttempts.
	skippedReply = "Couldn't process this message. It's kept for a replay."
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
// Captures left unfinished. One goroutine at a time may call Poll, and one may
// call ProcessInbox or Process; Run does both. CatchUp and ServeHealth are safe
// for concurrent use.
type Bot struct {
	db       *sqlite.DB
	tg       *telegram.Client
	ownerID  int64
	classify ClassifyFunc
	offset   int64            // the next update_id to ask Telegram for; only Poll uses it
	timeNow  func() time.Time // time.Now, unless a test sets another clock
	polledAt atomic.Int64     // when the last successful getUpdates finished, in Unix nanoseconds; 0 before the first

	// repliedAlbumID is the media_group_id of the last album that got a
	// reply. Only Process uses it, so it is lost on a restart.
	repliedAlbumID string

	// mu is held while a Thought is captured, corrected, deleted or caught up
	// on, except while the catch-up waits for the classifier, so that two of
	// them never act on the same Thought at once.
	//
	// ponytail: one lock for every Thought, so a Capture can wait for one
	// catch-up Telegram call, up to 10s, before its classification starts;
	// lock per Thought if that delay ever shows.
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

// Run polls Telegram into the inbox in one goroutine, processes the inbox in
// another, and catches up at once and then every catchUpInterval in a third,
// until ctx is cancelled. So a backlog to process never holds up polling.
// Failures are logged and retried.
func (b *Bot) Run(ctx context.Context) {
	var wg sync.WaitGroup
	defer wg.Wait()
	wake := make(chan struct{}, 1) // told of each poll that succeeds
	wg.Go(func() { b.pollAndWake(ctx, wake) })
	wg.Go(func() { b.processInboxOnWake(ctx, wake) })
	wg.Go(func() { b.catchUpEveryInterval(ctx) })
}

// pollAndWake polls Telegram until ctx is cancelled, and wakes the processor
// after each poll that succeeds. After a failure, it waits retryDelay.
func (b *Bot) pollAndWake(ctx context.Context, wake chan<- struct{}) {
	for ctx.Err() == nil {
		if err := b.Poll(ctx); err != nil {
			if ctx.Err() == nil {
				slog.ErrorContext(ctx, "polling telegram", "err", err)
			}
			waitOrCancel(ctx, retryDelay)
			continue
		}
		select {
		case wake <- struct{}{}:
		default: // woken already, and yet to process
		}
	}
}

// processInboxOnWake processes the inbox at once and then each time it is
// woken, until ctx is cancelled. After a failure, it tries again after
// retryDelay.
func (b *Bot) processInboxOnWake(ctx context.Context, wake <-chan struct{}) {
	for ctx.Err() == nil {
		if err := b.ProcessInbox(ctx); err != nil {
			if ctx.Err() == nil {
				slog.ErrorContext(ctx, "processing inbox", "err", err)
			}
			waitOrCancel(ctx, retryDelay)
			continue
		}
		select {
		case <-ctx.Done():
		case <-wake:
		}
	}
}

// waitOrCancel returns once d has passed, or ctx is cancelled.
func waitOrCancel(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Poll long-polls Telegram once and stores the updates it returns in the
// inbox.
func (b *Bot) Poll(ctx context.Context) error {
	updates, err := b.tg.GetUpdates(ctx, b.offset, pollTimeout)
	if err != nil {
		return err
	}
	b.polledAt.Store(b.timeNow().UnixNano())
	for _, u := range updates {
		row := sqlite.InboxUpdate{ID: u.ID, Payload: u.Raw}
		// An edit carries the message's text too, so /delete must find it.
		if m := cmp.Or(u.Message, u.EditedMessage); m != nil {
			row.MessageID = m.ID
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

// ServeHealth answers 200 while the last successful getUpdates finished under
// staleAfter ago, and 503 otherwise.
func (b *Bot) ServeHealth(w http.ResponseWriter, _ *http.Request) {
	if b.timeNow().Sub(time.Unix(0, b.polledAt.Load())) >= staleAfter {
		http.Error(w, "no successful getUpdates in "+staleAfter.String(), http.StatusServiceUnavailable)
	}
}

// ProcessInbox processes the unprocessed inbox updates in order. It stops at
// the first failure, leaving that update and the ones after it for the next
// call. An update whose processing has begun maxAttempts times is skipped,
// so that it never stalls the ones behind it.
func (b *Bot) ProcessInbox(ctx context.Context) error {
	updates, err := b.db.GetUnprocessedUpdates(ctx)
	if err != nil {
		return err
	}
	for _, in := range updates {
		if in.Attempts >= maxAttempts {
			if err := b.skipUpdate(ctx, in); err != nil {
				return err
			}
			continue
		}
		// Counted first, so that a panic or a crash counts too.
		if err := b.db.CountAttempt(ctx, in.ID); err != nil {
			return err
		}
		if err := b.Process(ctx, in); err != nil {
			return fmt.Errorf("processing update %d: %w", in.ID, err)
		}
	}
	return nil
}

// skipUpdate marks update in failed, keeping it for a replay, logs an error,
// and tells the owner, in reply to their message if in carries one. A failure
// to tell them is logged.
func (b *Bot) skipUpdate(ctx context.Context, in sqlite.InboxUpdate) error {
	if err := b.db.MarkFailed(ctx, in.ID); err != nil {
		return err
	}
	slog.ErrorContext(ctx, "update skipped", "update_id", in.ID, "attempts", in.Attempts)

	// A payload that doesn't decode carries no message to reply to.
	var replyToID int64
	var update telegram.Update
	if err := json.Unmarshal(in.Payload, &update); err == nil {
		if m := b.findOwnerMessage(update); m != nil {
			replyToID = m.ID
		}
	}
	if _, err := b.tg.SendMessage(ctx, b.ownerID, skippedReply, replyToID); err != nil {
		slog.WarnContext(ctx, "telling of skipped update", "update_id", in.ID, "err", err)
	}
	return nil
}

// Process handles one inbox update: the owner's text message becomes a
// Thought, which is classified and gets an Acknowledgement, the owner's /kind
// or /topics corrects one, and their /delete deletes one. The owner's edits,
// messages with no text and other commands get a reply saying what Cermin
// takes, except service messages and an album's later photos. Those, and
// everyone else's updates, are only marked processed.
func (b *Bot) Process(ctx context.Context, in sqlite.InboxUpdate) error {
	var update telegram.Update
	if err := json.Unmarshal(in.Payload, &update); err != nil {
		return fmt.Errorf("decoding: %w", err)
	}

	// The owner's text is a Thought, unless it starts with /, which makes it a
	// command. An edit changes nothing, not even a command.
	m := b.findOwnerMessage(update)
	switch {
	case m == nil:
		slog.InfoContext(ctx, "update not captured", "update_id", in.ID)
		return b.db.MarkProcessed(ctx, in.ID)
	case update.EditedMessage != nil:
		return b.replyToEdit(ctx, in.ID, m)
	case strings.HasPrefix(m.Text, "/"):
		return b.runCommand(ctx, in.ID, m)
	case m.Text == "":
		return b.replyToNonText(ctx, in.ID, m)
	default:
		return b.captureThought(ctx, in.ID, m)
	}
}

// findOwnerMessage returns the message or edited message of update u if the
// owner sent it in their private chat with the bot, and nil otherwise. Only
// those count: a message ID from another chat could clash with a captured
// Thought's.
func (b *Bot) findOwnerMessage(u telegram.Update) *telegram.Message {
	m := cmp.Or(u.Message, u.EditedMessage)
	isOwner := m != nil && m.From != nil && m.From.ID == b.ownerID
	if !isOwner || m.Chat.ID != b.ownerID { // a private chat's ID is the user's
		return nil
	}
	return m
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

// reply sends text in reply to the owner's message m, from update updateID. A
// failure is logged and not retried, as a failed Acknowledgement is, so that
// it never stalls the inbox.
func (b *Bot) reply(ctx context.Context, updateID int64, m *telegram.Message, text string) {
	if _, err := b.tg.SendMessage(ctx, b.ownerID, text, m.ID); err != nil {
		slog.WarnContext(ctx, "replying", "update_id", updateID, "err", err)
	}
}

// replyAndMarkProcessed sends text in reply to the owner's message m, and
// marks update updateID, which carried it, processed.
func (b *Bot) replyAndMarkProcessed(ctx context.Context, updateID int64, m *telegram.Message, text string) error {
	b.reply(ctx, updateID, m, text)
	return b.db.MarkProcessed(ctx, updateID)
}

// replyToEdit tells the owner, in reply to their edited message m, from
// update updateID, that edits aren't saved. The update is kept, marked
// processed, if m carries a Thought, and removed from the inbox otherwise: m
// may have carried a Thought deleted since, and the edit holds its text.
func (b *Bot) replyToEdit(ctx context.Context, updateID int64, m *telegram.Message) error {
	_, isThought, err := b.db.GetThoughtByMessageID(ctx, m.ID)
	if err != nil {
		return err
	}
	if isThought {
		return b.replyAndMarkProcessed(ctx, updateID, m, editReply)
	}
	b.reply(ctx, updateID, m, editReply)
	return b.db.DeleteUpdate(ctx, updateID)
}

// replyToNonText tells the owner, in reply to their message m with no text,
// from update updateID, that Cermin takes text only, and marks the update
// processed. Of an album, only the first photo or video gets the reply, and a
// service message, such as your pin of a message, gets none. An album split
// by a restart gets a second reply.
func (b *Bot) replyToNonText(ctx context.Context, updateID int64, m *telegram.Message) error {
	isAlbumReplied := m.MediaGroupID != "" && m.MediaGroupID == b.repliedAlbumID
	if m.IsService() || isAlbumReplied {
		return b.db.MarkProcessed(ctx, updateID)
	}
	b.repliedAlbumID = m.MediaGroupID
	return b.replyAndMarkProcessed(ctx, updateID, m, textOnlyReply)
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
	archived, err := b.db.GetArchivedTopicNames(ctx)
	if err != nil {
		return false, err
	}
	c, err := b.classify(ctx, t.Text, b.db.Kinds(), topics)
	if err != nil {
		return false, err
	}

	c.ThoughtID = t.ID
	c.Source = cermin.SourceCermin
	c.Topics = cermin.KeepTopics(c.Topics, archived)
	if len(c.KeptTopics()) == 0 {
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
