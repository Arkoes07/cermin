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

// ErrMessageNotFound is EditMessageText's error for a message that is gone,
// as when the user deleted it.
var ErrMessageNotFound = errors.New("telegram: message to edit not found")

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

// Update is one event the Bot API reports, with its own update_id. Cermin
// doesn't ask for particular kinds, so it gets the default set: a new
// message, an edit of one, and other kinds that it only marks processed,
// such as my_chat_member when you block the bot.
type Update struct {
	ID            int64    `json:"update_id"`
	Message       *Message `json:"message"`        // nil for other kinds of update, such as edits
	EditedMessage *Message `json:"edited_message"` // the message as edited; nil unless this is an edit

	// Raw is the whole update, as Telegram sent it, except for the copies of
	// other messages in its message or edited message: reply_to_message and
	// pinned_message are cut down to {"message_id": N}, and quote is dropped.
	// So a stored update never keeps the text of a message deleted since.
	Raw json.RawMessage `json:"-"`
}

// Message is a Telegram message.
type Message struct {
	ID             int64    `json:"message_id"`
	From           *User    `json:"from"` // nil in channels
	Chat           Chat     `json:"chat"`
	Date           int64    `json:"date"` // Unix time
	Text           string   `json:"text"` // "" if the message isn't text
	ReplyToMessage *Message `json:"reply_to_message"`
	MediaGroupID   string   `json:"media_group_id"` // the album the message's photo or video is in; "" if none

	// The service messages you can cause in a private chat with a bot. Each
	// is nil unless the message is that one.
	PinnedMessage          *Message  `json:"pinned_message"` // your pin of a message
	AutoDeleteTimerChanged *struct{} `json:"message_auto_delete_timer_changed"`
	ChatBackgroundSet      *struct{} `json:"chat_background_set"`
}

// IsService reports whether m is one of the service messages you can cause in
// a private chat with a bot: your pin of a message, a change to the chat's
// auto-delete timer, or a new chat background.
func (m *Message) IsService() bool {
	return m.PinnedMessage != nil || m.AutoDeleteTimerChanged != nil || m.ChatBackgroundSet != nil
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
// Telegram then drops it for good. Each update's Raw has the copies of other
// messages cut down.
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
		cut, err := cutCopies(raw)
		if err != nil {
			return nil, fmt.Errorf("telegram: decoding update %d: %w", u.ID, err)
		}
		u.Raw = cut
		updates = append(updates, u)
	}
	return updates, nil
}

// cutCopies returns the update raw with the copies of other messages in its
// message or edited message cut down, as Update.Raw describes. An update
// with none is returned as it is.
func cutCopies(raw json.RawMessage) (json.RawMessage, error) {
	var update map[string]json.RawMessage
	if err := json.Unmarshal(raw, &update); err != nil {
		return nil, err
	}
	isCut := false
	for _, field := range []string{"message", "edited_message"} {
		if update[field] == nil {
			continue
		}
		cut, isMessageCut, err := cutMessageCopies(update[field])
		if err != nil {
			return nil, err
		}
		update[field] = cut
		isCut = isCut || isMessageCut
	}
	if !isCut {
		return raw, nil
	}
	return encodeRaw(update)
}

// cutMessageCopies returns the message raw with reply_to_message and
// pinned_message cut down to their message_id, and quote dropped, and
// reports whether it had any of them to cut.
func cutMessageCopies(raw json.RawMessage) (json.RawMessage, bool, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false, err
	}
	if m["reply_to_message"] == nil && m["pinned_message"] == nil && m["quote"] == nil {
		return raw, false, nil
	}

	for _, field := range []string{"reply_to_message", "pinned_message"} {
		if m[field] == nil {
			continue
		}
		var copied struct {
			ID int64 `json:"message_id"`
		}
		if err := json.Unmarshal(m[field], &copied); err != nil {
			return nil, false, err
		}
		m[field] = json.RawMessage(fmt.Sprintf(`{"message_id":%d}`, copied.ID))
	}
	delete(m, "quote")
	cut, err := encodeRaw(m)
	return cut, true, err
}

// encodeRaw encodes v as JSON, keeping <, > and & as they are rather than
// escaping them as json.Marshal does, so that text stays as Telegram sent it.
func encodeRaw(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// SendMessage sends text as plain text, with no parse mode, in reply to
// message replyToID, or to none if it is 0, and returns the sent message.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, replyToID int64) (Message, error) {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	params := map[string]any{"chat_id": chatID, "text": text}
	if replyToID != 0 {
		params["reply_parameters"] = map[string]any{
			"message_id": replyToID,
			// Otherwise a deleted original would fail the send for good.
			"allow_sending_without_reply": true,
		}
	}
	var sent Message
	if err := c.call(ctx, "sendMessage", params, &sent); err != nil {
		return Message{}, err
	}
	return sent, nil
}

// EditMessageText replaces the text of message messageID, which the bot sent
// in chatID, with text, as plain text with no parse mode. A message that
// already reads text is left as it is. A message that is gone fails with
// ErrMessageNotFound.
func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	params := map[string]any{"chat_id": chatID, "message_id": messageID, "text": text}
	var edited Message
	err := c.call(ctx, "editMessageText", params, &edited)
	// Telegram refuses an edit that changes nothing, as when the same
	// Correction is sent twice, and an edit of a message that is gone. Only
	// the description tells them apart.
	apiErr, isAPIErr := errors.AsType[*apiError](err)
	switch {
	case !isAPIErr:
		return err
	case strings.Contains(apiErr.description, "message is not modified"):
		return nil
	case strings.Contains(apiErr.description, "message to edit not found"):
		return ErrMessageNotFound
	default:
		return err
	}
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
		return fmt.Errorf("telegram: %s: %w", method, stripURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s: %w", method, stripURL(err))
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

// stripURL drops the request URL from err, because the URL carries the bot
// token.
func stripURL(err error) error {
	if urlErr, ok := errors.AsType[*url.Error](err); ok {
		return urlErr.Err
	}
	return err
}
