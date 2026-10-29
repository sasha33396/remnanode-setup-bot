package webconsole

import (
	"testing"
	"time"
)

func TestTerminalTicketIsOneTimeAndUserBound(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := newTicketStore(func() time.Time { return now })
	token, err := store.issue(terminalTicket{UserID: 42, Password: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.take(token, 7); ok {
		t.Fatal("ticket was accepted for another operator")
	}
	value, ok := store.take(token, 42)
	if !ok || string(value.Password) != "secret" {
		t.Fatalf("ticket = %#v, %v", value, ok)
	}
	if _, ok := store.take(token, 42); ok {
		t.Fatal("ticket was accepted twice")
	}
}

func TestExpiredTerminalTicketIsCleared(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	store := newTicketStore(func() time.Time { return now })
	token, err := store.issue(terminalTicket{UserID: 42, Password: []byte("secret")})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := store.take(token, 42); ok || len(store.values) != 0 {
		t.Fatal("expired ticket remains available")
	}
}
