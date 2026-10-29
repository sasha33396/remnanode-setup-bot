package webconsole

import (
	"errors"
	"net/netip"
	"sync"
	"time"
)

type terminalTicket struct {
	UserID       int64
	PanelID      string
	NodeUUID     string
	NodeName     string
	Address      netip.Addr
	Managed      bool
	DeploymentID string
	Password     []byte
	ExpiresAt    time.Time
}

func (t *terminalTicket) clear() {
	for index := range t.Password {
		t.Password[index] = 0
	}
	t.Password = nil
}

type ticketStore struct {
	mu      sync.Mutex
	values  map[string]terminalTicket
	now     func() time.Time
	maximum int
}

func newTicketStore(now func() time.Time) *ticketStore {
	return &ticketStore{values: make(map[string]terminalTicket), now: now, maximum: 1000}
}

func (s *ticketStore) issue(value terminalTicket) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	if len(s.values) >= s.maximum {
		value.clear()
		return "", errors.New("too many pending terminal sessions")
	}
	token, err := randomToken(32)
	if err != nil {
		value.clear()
		return "", err
	}
	value.ExpiresAt = s.now().Add(time.Minute)
	s.values[token] = value
	return token, nil
}

func (s *ticketStore) take(token string, userID int64) (terminalTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	value, found := s.values[token]
	if !found || value.UserID != userID {
		return terminalTicket{}, false
	}
	delete(s.values, token)
	return value, true
}

func (s *ticketStore) sweepLocked() {
	now := s.now()
	for token, value := range s.values {
		if !value.ExpiresAt.After(now) {
			value.clear()
			delete(s.values, token)
		}
	}
}
