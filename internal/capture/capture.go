// Package capture turns the owner's Telegram messages into Thoughts and
// classifies them. Every update is stored in the inbox before anything else
// happens and processed from there, so a crash or a failed reply never loses
// one.
package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/arkoes07/cermin/internal/classify"
	"github.com/arkoes07/cermin/internal/store"
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
)

// ClassifyFunc gives a Thought's text a Classification, with one of kinds
// and candidate Topics that reuse the existing topics where one fits, and
// returns the Thought's language. (*classify.Classifier).Classify is one.
type ClassifyFunc func(
	ctx context.Context,
	text string,
	kinds []store.Kind,
	topics []string,
) (c store.Classification, language string, err error)

// Bot moves Telegram updates into the inbox and captures the owner's Thoughts
// from it. Its methods must not be called concurrently.
type Bot struct {
	db       *store.DB
	tg       *telegram.Client
	ownerID  int64
	classify ClassifyFunc
	offset   int64 // the next update_id to ask Telegram for
}

// New returns a Bot that captures the Thoughts of the Telegram user ownerID,
// ignoring everyone else, and classifies them with classify.
func New(db *store.DB, tg *telegram.Client, ownerID int64, classify ClassifyFunc) *Bot {
	return &Bot{db: db, tg: tg, ownerID: ownerID, classify: classify}
}

// Run processes the inbox and polls Telegram for more, until ctx is
// cancelled. Failures are logged and retried.
func (b *Bot) Run(ctx context.Context) {
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
		row := store.InboxUpdate{ID: u.ID, Payload: u.Raw}
		if m := u.Message; m != nil {
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
// Thought, which is classified and gets an Acknowledgement. Anything else is
// only marked processed.
func (b *Bot) Process(ctx context.Context, in store.InboxUpdate) error {
	var update telegram.Update
	if err := json.Unmarshal(in.Payload, &update); err != nil {
		return fmt.Errorf("decoding: %w", err)
	}
	// Only the owner's plain text in their private chat with the bot is a
	// Thought. Edits and non-text messages carry none, text starting with / is
	// a command, and a message ID from another chat could clash with a
	// captured Thought's.
	m := update.Message
	isOwner := m != nil && m.From != nil && m.From.ID == b.ownerID
	isPrivate := isOwner && m.Chat.ID == b.ownerID // a private chat's ID is the user's
	isThought := isPrivate && m.Text != "" && !strings.HasPrefix(m.Text, "/")
	if !isThought {
		slog.InfoContext(ctx, "update not captured", "update_id", in.ID)
		return b.db.MarkProcessed(ctx, in.ID)
	}

	thought, err := b.db.SaveThought(ctx, store.Thought{
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
	// processed before a crash. A crash between sending and storing it still
	// sends a second one.
	if thought.AckMessageID == 0 {
		if err := b.tg.SendChatAction(ctx, m.Chat.ID, "typing"); err != nil {
			slog.WarnContext(ctx, "showing typing", "thought_id", thought.ID, "err", err)
		}
		text, err := b.classifyThought(ctx, thought)
		if err != nil {
			// A slow or failing classifier never fails a Capture: the
			// Thought is saved and stays unclassified for now.
			slog.WarnContext(ctx, "classifying thought", "thought_id", thought.ID, "err", err)
			text = savedAck
		}
		ack, err := b.tg.SendMessage(
			ctx,
			m.Chat.ID,
			text,
			m.ID,
		)
		if err != nil {
			return err
		}
		if err := b.db.SetAckMessageID(ctx, thought.ID, ack.ID); err != nil {
			return err
		}
		slog.InfoContext(ctx, "thought captured",
			"thought_id", thought.ID,
			"update_id", in.ID,
			"ack_message_id", ack.ID,
		)
	}
	return b.db.MarkProcessed(ctx, in.ID)
}

// classifyThought gives a Thought its Classification and returns the
// Acknowledgement that shows it. It gives up after classifyTimeout.
func (b *Bot) classifyThought(ctx context.Context, t store.Thought) (string, error) {
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
	topics, err := b.db.TopicNames(ctx)
	if err != nil {
		return "", err
	}
	c, language, err := b.classify(
		ctx,
		t.Text,
		kinds,
		topics,
	)
	if err != nil {
		return "", err
	}
	c.ThoughtID = t.ID
	c.Source = "cermin"
	c.Topics = classify.Keep(c.Topics)
	if len(c.Topics) == 0 {
		return "", errors.New("classified with no topic")
	}
	stored, err := b.db.SaveClassification(ctx, c, language)
	if err != nil {
		return "", err
	}
	return ackText(kinds, stored)
}

// ackText is the Acknowledgement of a Thought classified as c, such as
// "💡 idea · business, retail": the Kind and the kept Topics, best first.
func ackText(kinds []store.Kind, c store.Classification) (string, error) {
	i := slices.IndexFunc(kinds, func(k store.Kind) bool { return k.Name == c.Kind })
	if i < 0 {
		return "", fmt.Errorf("classified with unknown kind %q", c.Kind)
	}
	topics := []string{}
	for _, t := range c.Topics {
		if t.Kept {
			topics = append(topics, t.Name)
		}
	}
	return kinds[i].Emoji + " " + c.Kind + " · " + strings.Join(topics, ", "), nil
}
