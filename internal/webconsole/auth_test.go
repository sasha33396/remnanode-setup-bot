package webconsole

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseTelegramUserIDAcceptsNumberAndString(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		want int64
	}{
		{name: "number", raw: `987654321`, want: 987654321},
		{name: "string", raw: `"987654321"`, want: 987654321},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseTelegramUserID(json.RawMessage(test.raw))
			if err != nil || got != test.want {
				t.Fatalf("parseTelegramUserID() = %d, %v; want %d", got, err, test.want)
			}
		})
	}
}

func TestParseTelegramUserIDRejectsMissingAndInvalidValues(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`0`), json.RawMessage(`"invalid"`), json.RawMessage(`{}`)} {
		if got, err := parseTelegramUserID(raw); err == nil || got != 0 {
			t.Fatalf("parseTelegramUserID(%s) = %d, %v", raw, got, err)
		}
	}
}

func TestOIDCLoginAttemptCookieRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := &Server{sessionKey: []byte(strings.Repeat("s", 32)), now: func() time.Time { return now }}
	attempt, err := newOIDCLoginAttempt(now)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.State == "" || attempt.Nonce == "" || attempt.Verifier == "" || attempt.State == attempt.Nonce {
		t.Fatalf("invalid OIDC attempt: %#v", attempt)
	}
	value, err := server.signOIDCLoginAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/auth/oidc/callback", nil)
	request.AddCookie(&http.Cookie{Name: loginStateCookie, Value: value})
	got, err := server.readOIDCLoginAttempt(request)
	if err != nil || got != attempt {
		t.Fatalf("readOIDCLoginAttempt() = %#v, %v", got, err)
	}
}

func TestOIDCLoginAttemptRejectsTamperingAndExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	server := &Server{sessionKey: []byte(strings.Repeat("s", 32)), now: func() time.Time { return now }}
	attempt, err := newOIDCLoginAttempt(now)
	if err != nil {
		t.Fatal(err)
	}
	value, err := server.signOIDCLoginAttempt(attempt)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://nodes.example.com/auth/oidc/callback", nil)
	request.AddCookie(&http.Cookie{Name: loginStateCookie, Value: "x" + value})
	if _, err := server.readOIDCLoginAttempt(request); err == nil {
		t.Fatal("tampered OIDC attempt was accepted")
	}
	server.now = func() time.Time { return now.Add(11 * time.Minute) }
	request = httptest.NewRequest(http.MethodGet, "https://nodes.example.com/auth/oidc/callback", nil)
	request.AddCookie(&http.Cookie{Name: loginStateCookie, Value: value})
	if _, err := server.readOIDCLoginAttempt(request); err == nil {
		t.Fatal("expired OIDC attempt was accepted")
	}
}
