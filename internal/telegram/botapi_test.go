package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestBotAPISkipsFailedUpdateInsteadOfRestartLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	offsets := make(chan int64, 3)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload getUpdatesRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		offsets <- payload.Offset
		writer.Header().Set("Content-Type", "application/json")
		calls++
		if calls == 1 {
			_, _ = writer.Write([]byte(`{"ok":true,"result":[{"update_id":10,"message":{"message_id":1,"from":{"id":42},"chat":{"id":42},"text":"poison"}}]}`))
			return
		}
		_, _ = writer.Write([]byte(`{"ok":true,"result":[]}`))
		cancel()
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client := &BotAPI{baseURL: baseURL, httpClient: server.Client()}
	if err := client.Run(ctx, failingUpdateHandler{}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	first, second := <-offsets, <-offsets
	if first != 0 || second != 11 {
		t.Fatalf("poll offsets = %d, %d; want 0, 11", first, second)
	}
}

type failingUpdateHandler struct{}

func (failingUpdateHandler) Handle(context.Context, Update) error {
	return errors.New("presentation failed")
}

func TestBotAPISendsInlineKeyboard(t *testing.T) {
	var requestPath string
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestPath = request.URL.Path
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true,"result":{"message_id":7,"from":{"id":1},"chat":{"id":42},"text":"sent"}}`))
	}))
	defer server.Close()
	baseURL, err := url.Parse(server.URL + "/bot-test-token/")
	if err != nil {
		t.Fatal(err)
	}
	client := &BotAPI{baseURL: baseURL, httpClient: server.Client()}

	message, err := client.SendMessage(context.Background(), 42, "choose", Keyboard{Inline: [][]Button{{{Text: "Select", CallbackData: "add:host:nonce:0"}}}})
	if err != nil {
		t.Fatalf("SendMessage() error = %v", err)
	}
	if requestPath != "/bot-test-token/sendMessage" {
		t.Fatalf("request path = %q", requestPath)
	}
	if message.ID != 7 || message.ChatID != 42 {
		t.Fatalf("message = %#v", message)
	}
	markup, ok := payload["reply_markup"].(map[string]any)
	if !ok || markup["inline_keyboard"] == nil {
		t.Fatalf("reply markup = %#v", payload["reply_markup"])
	}
}

func TestBotAPINetworkErrorsNeverExposeToken(t *testing.T) {
	const secretToken = "123456:very-secret-token"
	baseURL, err := url.Parse("http://127.0.0.1:1/bot" + secretToken + "/")
	if err != nil {
		t.Fatal(err)
	}
	client := &BotAPI{baseURL: baseURL, httpClient: &http.Client{Timeout: 100 * time.Millisecond}}
	_, err = client.SendMessage(context.Background(), 42, "test", Keyboard{})
	if err == nil {
		t.Fatal("SendMessage() error = nil")
	}
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "very-secret") {
		t.Fatalf("error exposed Bot token: %v", err)
	}
}
