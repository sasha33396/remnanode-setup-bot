package webconsole

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
