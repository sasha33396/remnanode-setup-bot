package webconsole

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestVerifyTelegramLogin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	values := url.Values{
		"id": {"12345"}, "first_name": {"Test"}, "last_name": {"Operator"},
		"username": {"operator"}, "auth_date": {"1800000000"}, "state": {"local-state"},
	}
	signTelegram(values, "bot-token")
	got, err := verifyTelegramLogin(values, "bot-token", now)
	if err != nil || got.UserID != 12345 || got.Name != "Test Operator" {
		t.Fatalf("verifyTelegramLogin() = %#v, %v", got, err)
	}

	values.Set("first_name", "Changed")
	if _, err := verifyTelegramLogin(values, "bot-token", now); err == nil {
		t.Fatal("tampered Telegram login was accepted")
	}
}

func TestVerifyTelegramLoginRejectsExpiredData(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	values := url.Values{"id": {"12345"}, "auth_date": {"1799999000"}}
	signTelegram(values, "bot-token")
	if _, err := verifyTelegramLogin(values, "bot-token", now); err == nil {
		t.Fatal("expired Telegram login was accepted")
	}
}

func signTelegram(values url.Values, token string) {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "hash" && key != "state" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+values.Get(key))
	}
	secret := sha256.Sum256([]byte(token))
	mac := hmac.New(sha256.New, secret[:])
	_, _ = mac.Write([]byte(strings.Join(parts, "\n")))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))
}
