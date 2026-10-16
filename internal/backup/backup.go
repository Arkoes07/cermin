// Package backup watches Litestream's replication of the database to R2. When
// changes have waited an hour to reach R2 it tells the owner in Telegram, and
// it tells them again when backups catch up.
package backup

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/arkoes07/cermin/internal/litestream"
	"github.com/arkoes07/cermin/internal/telegram"
)

const (
	// checkInterval is how often Run checks the backups.
	checkInterval = time.Minute
	// behindAfter is how long changes wait to reach R2 before the owner is
	// told.
	behindAfter = time.Hour
	// behindText tells the owner that changes have waited behindAfter.
	behindText = "⚠️ Backups are behind: changes have waited over an hour to reach R2."
	// caughtUpText tells the owner that backups have caught up since behindText.
	caughtUpText = "✅ Backups have caught up."
)

// unsyncedSeconds is how long changes have waited to reach R2, as of the last
// check.
var unsyncedSeconds = promauto.NewGauge(prometheus.GaugeOpts{
	Name: "cermin_backup_unsynced_seconds",
	Help: "How long changes have waited to reach R2, or 0.",
})

// Watch checks the backups and tells the owner when they fall behind and when
// they catch up. It remembers in memory, so a restart starts the count again.
// One goroutine at a time may call Check; Run does.
type Watch struct {
	litestream *litestream.Client
	tg         *telegram.Client
	ownerID    int64

	waitingSince time.Time // when the check first saw the changes waiting; zero if none are
	isBehindSent bool      // whether the owner's last message from the Watch is behindText
}

// New returns a Watch that asks Litestream about the backups through ls and
// messages ownerID through tg.
func New(ls *litestream.Client, tg *telegram.Client, ownerID int64) *Watch {
	return &Watch{litestream: ls, tg: tg, ownerID: ownerID}
}

// Run checks the backups at once and then every checkInterval, until ctx is
// cancelled.
func (w *Watch) Run(ctx context.Context) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		w.Check(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Check asks Litestream whether changes are waiting to reach R2, as of now. A
// failed read counts as waiting: a backup Cermin can't confirm isn't
// confirmed. Once changes have waited behindAfter, Check tells the owner, and
// when they catch up after that, it tells them so. Each message goes once per
// stretch of waiting; a failed send is logged, and the next check retries it.
func (w *Watch) Check(ctx context.Context, now time.Time) {
	pos, err := w.litestream.Sync(ctx)
	if err != nil {
		slog.WarnContext(ctx, "asking litestream about backups", "err", err)
	}
	if err == nil && pos.ReplicatedTXID == pos.TXID {
		w.waitingSince = time.Time{}
		unsyncedSeconds.Set(0)
		if w.isBehindSent && w.send(ctx, caughtUpText) {
			w.isBehindSent = false
		}
		return
	}

	if w.waitingSince.IsZero() {
		w.waitingSince = now
	}
	waited := now.Sub(w.waitingSince)
	unsyncedSeconds.Set(waited.Seconds())
	if waited >= behindAfter && !w.isBehindSent && w.send(ctx, behindText) {
		w.isBehindSent = true
	}
}

// send sends the owner text, and reports whether it went. A failure is
// logged.
func (w *Watch) send(ctx context.Context, text string) bool {
	if _, err := w.tg.SendMessage(ctx, w.ownerID, text, 0); err != nil {
		slog.ErrorContext(ctx, "telling the owner about backups", "err", err)
		return false
	}
	return true
}
