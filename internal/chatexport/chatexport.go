// Package chatexport reads a WhatsApp chat export: the _chat.txt file the iOS
// app writes for a chat exported without media, in one format, where a
// message starts with a header, the day, month, two-digit year and 24-hour
// time in WIB, then the sender:
//
//	[05/10/26, 08.02.11] Your Name: Code review feels slow.
package chatexport

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/arkoes07/cermin/internal/cermin"
)

// header is a line that starts a message: its time, in timeLayout, its
// sender, then the first line of its text.
var header = regexp.MustCompile(`^\[(\d\d/\d\d/\d\d, \d\d\.\d\d\.\d\d)\] [^:]+: (.*)$`)

// timeLayout is how a header writes a message's time.
const timeLayout = "02/01/06, 15.04.05"

// Message is one message in a chat export.
type Message struct {
	ThoughtAt time.Time // when it was sent, in UTC
	Text      string
}

// Parse reads the messages of the chat export r, in order. Each line that
// isn't a header continues the message before it, so a message that spans
// several lines stays one.
func Parse(r io.Reader) ([]Message, error) {
	messages := []Message{}
	scanner := bufio.NewScanner(r)
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		m := header.FindStringSubmatch(line)
		if m == nil {
			if len(messages) == 0 {
				return nil, fmt.Errorf("line %d: no message header", n)
			}
			messages[len(messages)-1].Text += "\n" + line
			continue
		}
		at, err := time.ParseInLocation(timeLayout, m[1], cermin.WIB)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		messages = append(messages, Message{ThoughtAt: at.UTC(), Text: m[2]})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return messages, nil
}
