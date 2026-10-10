// Package telegram is a small client for the Telegram Bot API: only the
// methods and fields Cermin uses.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// APIURL is the Telegram Bot API's address.
const APIURL = "https://api.telegram.org"

// sendTimeout bounds every call except the long poll.
const sendTimeout = 10 * time.Second

// Client calls the Bot API as one bot.
type Client struct {
	apiURL string
	token  string
}

// New returns a client for the bot with the given token. apiURL is APIURL,
// or a fake's address in tests.
func New(apiURL, token string) *Client {
	return &Client{apiURL: apiURL, token: token}
}

// Update is an incoming update.
type Update struct {
	ID            int64           `json:"update_id"`
	Message       *Message        `json:"message"`        // nil for other kinds of update, such as edits
	EditedMessage *Message        `json:"edited_message"` // the message as edited; nil unless this is an edit
	Raw           json.RawMessage `json:"-"`              // the whole update, as Telegram sent it
}

// Message is a Telegram message.
type Message struct {
	ID             int64    `json:"message_id"`
	From           *User    `json:"from"` // nil in channels
	Chat           Chat     `json:"chat"`
	Date           int64    `json:"date"` // Unix time
	Text           string   `json:"text"` // "" if the message isn't text
	ReplyToMessage *Message `json:"reply_to_message"`
}

// ReplyToID returns the ID of the message m replies to, or 0 if m is nil or
// isn't a reply.
func (m *Message) ReplyToID() int64 {
	if m == nil || m.ReplyToMessage == nil {
		return 0
	}
	return m.ReplyToMessage.ID
}

// User is a Telegram user or bot.
type User struct {
	ID int64 `json:"id"`
}

// Chat is a Telegram chat.
type Chat struct {
	ID int64 `json:"id"`
}

// GetUpdates long-polls for updates from offset on, waiting up to timeout for
// one to arrive. Asking for an offset confirms every earlier update, and
// Telegram then drops it for good.
func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]Update, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+sendTimeout)
	defer cancel()

	var raws []json.RawMessage
	params := map[string]any{"offset": offset, "timeout": int(timeout / time.Second)}
	if err := c.call(ctx, "getUpdates", params, &raws); err != nil {
		return nil, err
	}
	updates := make([]Update, 0, len(raws))
	for _, raw := range raws {
		var u Update
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, fmt.Errorf("telegram: decoding update: %w", err)
		}
		u.Raw = raw
		updates = append(updates, u)
	}
	return updates, nil
}

// SendMessage sends text as plain text, with no parse mode, in reply to
// message replyToID, and returns the sent message.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyToID int64) (Message, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	params := map[string]any{
		"chat_id": chatID,
		"text":    text,
		"reply_parameters": map[string]any{
			"message_id": replyToID,
			// Otherwise a deleted original would fail the send for good.
			"allow_sending_without_reply": true,
		},
	}
	var sent Message
	if err := c.call(ctx, "sendMessage", params, &sent); err != nil {
		return Message{}, err
	}
	return sent, nil
}

// EditMessageText replaces the text of message messageID, which the bot sent
// in chatID, with text, as plain text with no parse mode. A message that
// already reads text is left as it is.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	params := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	var edited Message
	err := c.call(ctx, "editMessageText", params, &edited)
	// Telegram refuses an edit that changes nothing, as when the same
	// Correction is sent twice, and only its description says so.
	apiErr, isAPIErr := errors.AsType[*apiError](err)
	isUnchanged := isAPIErr && strings.Contains(apiErr.description, "message is not modified")
	if isUnchanged {
		return nil
	}
	return err
}

// SendChatAction shows action, such as "typing", in the chat until the bot
// sends a message there or a few seconds pass.
func (c *Client) SendChatAction(ctx context.Context, chatID int64, action string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	var ok bool
	return c.call(ctx, "sendChatAction", map[string]any{"chat_id": chatID, "action": action}, &ok)
}

// call invokes a Bot API method and decodes its result into result.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram: %s: encoding request: %w", method, err)
	}
	endpoint := c.apiURL + "/bot" + c.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, withoutURL(err))
	}
	defer resp.Body.Close()

	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram: %s: decoding response with status %d: %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		return &apiError{method: method, status: resp.StatusCode, description: r.Description}
	}
	if err := json.Unmarshal(r.Result, result); err != nil {
		return fmt.Errorf("telegram: %s: decoding result: %w", method, err)
	}
	return nil
}

// apiError is the Bot API's refusal of a call.
type apiError struct {
	method      string
	status      int
	description string // Telegram's reason, such as "Bad Request: message to edit not found"
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram: %s: status %d: %s", e.method, e.status, e.description)
}

// withoutURL drops the request URL from err, because the URL carries the bot
// token.
func withoutURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}
