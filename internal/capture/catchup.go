package capture

import (
	"context"
	"log/slog"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

const (
	// catchUpInterval is how often Run catches up.
	catchUpInterval = time.Minute
	// catchUpClassifyTimeout is how long the catch-up loop waits for a
	// Thought's Classification. No Acknowledgement waits on it, so a slow
	// classifier gets time to finish.
	catchUpClassifyTimeout = 60 * time.Second
	// catchUpLimit is the most Thoughts one catch-up classifies, to cap cost.
	catchUpLimit = 20
)

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
