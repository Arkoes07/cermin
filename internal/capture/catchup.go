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
	// catchUpLimit is the most Thoughts one catch-up classifies, and embeds,
	// to cap cost, and the most Acknowledgements it brings up to date, and
	// edits to show a deletion, to stay under Telegram's rate limits. The rest
	// wait for the next catch-up.
	catchUpLimit = 20
	// classifyMaxBackoff is the longest the catch-up waits to try again to
	// classify a Thought it failed on.
	classifyMaxBackoff = 24 * time.Hour
	// classifyRetrySlack comes off every wait, so that scheduler jitter never
	// puts a Thought due at one catch-up off to the next.
	classifyRetrySlack = 30 * time.Second
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
// brings up to catchUpLimit out-of-date Acknowledgements up to date, sending
// the ones never sent, and edits up to catchUpLimit deleted Thoughts'
// Acknowledgements to 🗑 deleted where that failed. Then it classifies up to
// catchUpLimit Thoughts that have no Classification and are due, oldest
// first, and edits their Acknowledgements to show it. A Thought it fails to
// classify is logged and postponed, ever longer, so that one the classifier
// always fails on costs few paid calls. Last, it embeds up to catchUpLimit
// Thoughts that have no vector from the embedding model, oldest first.
func (b *Bot) CatchUp(ctx context.Context) error {
	now := b.timeNow()
	if err := b.syncOutOfDateAcks(ctx); err != nil {
		return err
	}
	if err := b.showDeletions(ctx); err != nil {
		return err
	}
	if err := b.classifyDueThoughts(ctx, now); err != nil {
		return err
	}
	return b.embedLateThoughts(ctx)
}

// syncOutOfDateAcks brings up to catchUpLimit out-of-date Acknowledgements up
// to date, oldest first, sending the ones never sent. A failure is logged,
// and the next catch-up retries it.
func (b *Bot) syncOutOfDateAcks(ctx context.Context) error {
	outOfDate, err := b.db.GetThoughtsWithOutOfDateAck(ctx, catchUpLimit)
	if err != nil {
		return err
	}
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
	return nil
}

// showDeletions edits up to catchUpLimit deleted Thoughts' Acknowledgements
// to 🗑 deleted, where /delete failed to. It takes no lock: should a /delete
// make the same edit meanwhile, the second edit changes nothing.
func (b *Bot) showDeletions(ctx context.Context) error {
	ackIDs, err := b.db.GetDeletedAckIDs(ctx, catchUpLimit)
	if err != nil {
		return err
	}
	for _, id := range ackIDs {
		b.showDeletion(ctx, id)
	}
	return nil
}

// classifyDueThoughts classifies up to catchUpLimit Thoughts that have no
// Classification and are due at now, the start of the catch-up, oldest
// first, and edits their Acknowledgements to show it. A Thought it fails to
// classify is logged and postponed.
func (b *Bot) classifyDueThoughts(ctx context.Context, now time.Time) error {
	// Under the lock no Capture is under way, so none of these Thoughts is
	// being classified by one.
	b.mu.Lock()
	due, err := b.db.GetThoughtsDueForClassification(ctx, now, catchUpLimit)
	b.mu.Unlock()
	if err != nil {
		return err
	}
	for _, t := range due {
		// Unlocked, so that a slow classifier never holds up a Capture.
		isSaved, err := b.classifyThought(ctx, t, catchUpClassifyTimeout)
		if err != nil {
			if ctx.Err() == nil { // a shutdown is no fault of the Thought's
				b.postponeClassification(ctx, now, t, err)
			}
			continue
		}
		// With none saved there is nothing new to show: the Thought is gone,
		// or whatever classified it shows that, or the next catch-up does.
		if isSaved {
			b.showLateClassification(ctx, t.ID)
		}
	}
	return nil
}

// postponeClassification logs classifyErr, the catch-up's failure to classify
// Thought t, and postpones t's next try: to 1, 2, 4 … minutes after now, the start of
// the catch-up, doubling with each failure up to classifyMaxBackoff.
func (b *Bot) postponeClassification(ctx context.Context, now time.Time, t cermin.Thought, classifyErr error) {
	// Only the catch-up writes the count, so t's is still current.
	failures := t.ClassifyFailures + 1
	backoff := time.Minute
	for range failures - 1 {
		// Capped as it doubles, so that it never overflows.
		backoff = min(2*backoff, classifyMaxBackoff)
	}
	retryAt := now.Add(backoff - classifyRetrySlack)
	slog.WarnContext(ctx, "classifying thought late", "thought_id", t.ID, "retry_at", retryAt, "err", classifyErr)
	if err := b.db.PostponeClassification(ctx, t.ID, failures, retryAt); err != nil {
		slog.WarnContext(ctx, "postponing classification", "thought_id", t.ID, "err", err)
	}
}

// showLateClassification brings the Acknowledgement of Thought thoughtID up
// to date to show the Classification the catch-up just saved, even during
// shutdown. A failure is logged, and the next catch-up retries it.
func (b *Bot) showLateClassification(ctx context.Context, thoughtID int64) {
	slog.InfoContext(ctx, "thought classified late", "thought_id", thoughtID)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.syncAck(context.WithoutCancel(ctx), thoughtID); err != nil {
		slog.WarnContext(ctx, "updating acknowledgement", "thought_id", thoughtID, "err", err)
	}
}

// embedLateThoughts embeds up to catchUpLimit Thoughts that have no vector
// from the embedding model, oldest first, in one call: those captured before
// embedding shipped, imported ones, and those a Capture failed to embed. A
// failure is logged, and the next catch-up tries again.
//
// ponytail: no backoff and no skipping, so a failing embedder costs one call
// a minute, and one text the API refuses holds up every Thought after it; back
// off per Thought, as classification does, if one text ever fails the whole
// batch.
func (b *Bot) embedLateThoughts(ctx context.Context) error {
	// Under the lock no Capture is under way, so none of these Thoughts is
	// being embedded by one.
	b.mu.Lock()
	thoughts, err := b.db.GetThoughtsWithoutEmbeddingByModel(ctx, b.embedModel, catchUpLimit)
	b.mu.Unlock()
	if err != nil || len(thoughts) == 0 {
		return err
	}

	ids := make([]int64, 0, len(thoughts))
	for _, t := range thoughts {
		ids = append(ids, t.ID)
	}
	// Unlocked, so that a slow embedder never holds up a Capture.
	if err := b.embedThoughts(ctx, thoughts); err != nil {
		if ctx.Err() == nil { // a shutdown is no fault of the embedder's
			slog.WarnContext(ctx, "embedding thoughts late", "thought_ids", ids, "err", err)
		}
		return nil
	}
	slog.InfoContext(ctx, "thoughts embedded late", "thought_ids", ids)
	return nil
}
