// Package cermin holds Cermin's domain: the Thoughts in the Corpus, their
// Classifications, and the rules about them that need no database, model or
// network call. It imports only the standard library.
package cermin

import "time"

// WIB is Western Indonesia Time, your home timezone, in which Thought times
// are read. It has no daylight saving time, so a fixed zone matches it and
// needs no tzdata.
var WIB = time.FixedZone("WIB", 7*60*60)

// Thought is a single free-text message captured into Cermin, in English or
// Bahasa Indonesia. It is never edited once captured; deleting it erases it
// completely.
type Thought struct {
	ID                  int64
	Text                string
	Source              string    // "telegram" or "whatsapp"
	TGMessageID         int64     // the Telegram message that carried it
	TGReplyToMessageID  int64     // the message it replied to; 0 if none
	AckMessageID        int64     // the Acknowledgement's message; 0 until sent
	AckClassificationID int64     // the Classification the Acknowledgement shows; 0 if none
	ThoughtAt           time.Time // Thought time
	ReceivedAt          time.Time // when Cermin stored it
	ClassifyFailures    int       // how many times the catch-up has failed to classify it
}

// EmbeddedThought is a Thought with its vector from one embedding model, a
// point that places its raw text by meaning.
type EmbeddedThought struct {
	Thought
	Vector []float32
}
