package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const maxTelegramResponseSize = 4 << 20

// UpdateHandler is implemented by Controller and kept separate for transport
// tests and future webhook adapters.
type UpdateHandler interface {
	Handle(context.Context, Update) error
}

// BotAPI is a small Telegram Bot API long-polling transport. The token never
// appears in returned errors.
type BotAPI struct {
	baseURL     *url.URL
	httpClient  *http.Client
	pollTimeout int
}

var _ Messenger = (*BotAPI)(nil)

func NewBotAPI(token string, timeout time.Duration) (*BotAPI, error) {
	token = strings.TrimSpace(token)
	if token == "" || timeout <= 0 || strings.ContainsAny(token, "/?#") || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return nil, errors.New("invalid Telegram Bot API configuration")
	}
	baseURL, _ := url.Parse("https://api.telegram.org/bot" + token + "/")
	pollTimeout := int(timeout.Seconds()) - 1
	if pollTimeout < 0 {
		pollTimeout = 0
	}
	if pollTimeout > 25 {
		pollTimeout = 25
	}
	return &BotAPI{
		baseURL:     baseURL,
		httpClient:  &http.Client{Timeout: timeout},
		pollTimeout: pollTimeout,
	}, nil
}

// Run polls updates until ctx is cancelled. Handler failures are returned so
// the process supervisor can restart from the last acknowledged offset.
func (b *BotAPI) Run(ctx context.Context, handler UpdateHandler) error {
	if handler == nil {
		return errors.New("Telegram update handler is required")
	}
	var offset int64
	for {
		updates, err := b.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for index := range updates {
			updateID := updates[index].UpdateID
			mapped := mapUpdate(updates[index])
			handleErr := handler.Handle(ctx, mapped)
			// Release Telegram message text promptly. Password messages have
			// already been copied into clearable session memory by Controller.
			if updates[index].Message != nil {
				updates[index].Message.Text = ""
			}
			if handleErr != nil {
				return errors.New("handle Telegram update failed")
			}
			if updateID >= offset {
				offset = updateID + 1
			}
		}
	}
}

func (b *BotAPI) SendMessage(ctx context.Context, chatID int64, text string, keyboard Keyboard) (Message, error) {
	var result messageWire
	payload := messageRequest{ChatID: chatID, Text: truncateUTF8(text, maxMessageBytes), ReplyMarkup: keyboardWire(keyboard)}
	if err := b.call(ctx, "sendMessage", payload, &result); err != nil {
		return Message{}, err
	}
	return mapMessage(result), nil
}

func (b *BotAPI) EditMessage(ctx context.Context, chatID int64, messageID int, text string, keyboard Keyboard) error {
	payload := editMessageRequest{ChatID: chatID, MessageID: messageID, Text: truncateUTF8(text, maxMessageBytes), ReplyMarkup: keyboardWire(keyboard)}
	var result json.RawMessage
	return b.call(ctx, "editMessageText", payload, &result)
}

func (b *BotAPI) DeleteMessage(ctx context.Context, chatID int64, messageID int) error {
	var result bool
	return b.call(ctx, "deleteMessage", deleteMessageRequest{ChatID: chatID, MessageID: messageID}, &result)
}

func (b *BotAPI) AnswerCallback(ctx context.Context, callbackID, text string) error {
	var result bool
	return b.call(ctx, "answerCallbackQuery", answerCallbackRequest{CallbackQueryID: callbackID, Text: safeLine(text, 180)}, &result)
}

func (b *BotAPI) getUpdates(ctx context.Context, offset int64) ([]updateWire, error) {
	var result []updateWire
	payload := getUpdatesRequest{Offset: offset, Timeout: b.pollTimeout, AllowedUpdates: []string{"message", "callback_query"}}
	if err := b.call(ctx, "getUpdates", payload, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (b *BotAPI) call(ctx context.Context, method string, payload, result any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("encode Telegram API request failed")
	}
	endpoint := b.baseURL.ResolveReference(&url.URL{Path: method})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(encoded))
	if err != nil {
		return errors.New("create Telegram API request failed")
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	response, err := b.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// net/http errors commonly contain the complete URL, including the Bot
		// token, so the original error must never cross this boundary.
		return errors.New("perform Telegram API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Telegram API returned HTTP %d", response.StatusCode)
	}
	var envelope apiEnvelope
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxTelegramResponseSize))
	if err := decoder.Decode(&envelope); err != nil || !envelope.OK {
		return errors.New("invalid Telegram API response")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid Telegram API response")
	}
	if err := json.Unmarshal(envelope.Result, result); err != nil {
		return errors.New("invalid Telegram API result")
	}
	return nil
}

type apiEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
}

type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates"`
}

type messageRequest struct {
	ChatID      int64  `json:"chat_id"`
	Text        string `json:"text"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

type editMessageRequest struct {
	ChatID      int64  `json:"chat_id"`
	MessageID   int    `json:"message_id"`
	Text        string `json:"text"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

type deleteMessageRequest struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int   `json:"message_id"`
}

type answerCallbackRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
}

type updateWire struct {
	UpdateID      int64              `json:"update_id"`
	Message       *messageWire       `json:"message"`
	CallbackQuery *callbackQueryWire `json:"callback_query"`
}

type messageWire struct {
	MessageID int      `json:"message_id"`
	From      userWire `json:"from"`
	Chat      chatWire `json:"chat"`
	Text      string   `json:"text"`
}

type callbackQueryWire struct {
	ID      string       `json:"id"`
	From    userWire     `json:"from"`
	Message *messageWire `json:"message"`
	Data    string       `json:"data"`
}

type userWire struct {
	ID int64 `json:"id"`
}

type chatWire struct {
	ID int64 `json:"id"`
}

type inlineKeyboardMarkup struct {
	InlineKeyboard [][]inlineKeyboardButton `json:"inline_keyboard"`
}

type inlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type replyKeyboardMarkup struct {
	Keyboard       [][]keyboardButton `json:"keyboard"`
	ResizeKeyboard bool               `json:"resize_keyboard"`
}

type keyboardButton struct {
	Text string `json:"text"`
}

func keyboardWire(keyboard Keyboard) any {
	if len(keyboard.Inline) > 0 {
		rows := make([][]inlineKeyboardButton, 0, len(keyboard.Inline))
		for _, row := range keyboard.Inline {
			buttons := make([]inlineKeyboardButton, 0, len(row))
			for _, button := range row {
				buttons = append(buttons, inlineKeyboardButton{Text: button.Text, CallbackData: button.CallbackData})
			}
			rows = append(rows, buttons)
		}
		return inlineKeyboardMarkup{InlineKeyboard: rows}
	}
	if len(keyboard.Reply) > 0 {
		rows := make([][]keyboardButton, 0, len(keyboard.Reply))
		for _, row := range keyboard.Reply {
			buttons := make([]keyboardButton, 0, len(row))
			for _, text := range row {
				buttons = append(buttons, keyboardButton{Text: text})
			}
			rows = append(rows, buttons)
		}
		return replyKeyboardMarkup{Keyboard: rows, ResizeKeyboard: true}
	}
	return nil
}

func mapUpdate(update updateWire) Update {
	result := Update{ID: update.UpdateID}
	if update.Message != nil {
		message := mapMessage(*update.Message)
		result.Message = &message
	}
	if update.CallbackQuery != nil {
		callback := CallbackQuery{ID: update.CallbackQuery.ID, FromUserID: update.CallbackQuery.From.ID, Data: update.CallbackQuery.Data}
		if update.CallbackQuery.Message != nil {
			message := mapMessage(*update.CallbackQuery.Message)
			callback.Message = &message
		}
		result.CallbackQuery = &callback
	}
	return result
}

func mapMessage(message messageWire) Message {
	return Message{ID: message.MessageID, ChatID: message.Chat.ID, FromUserID: message.From.ID, Text: message.Text}
}
