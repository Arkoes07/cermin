package capture

import "time"

// SetTimeNow makes b read the time from now, so that a test can run the
// catch-up at the times it chooses.
func (b *Bot) SetTimeNow(now func() time.Time) {
	b.timeNow = now
}
