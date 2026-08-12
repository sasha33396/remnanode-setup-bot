package certmanager

import (
	"context"
	"strconv"
	"strings"
)

// ScopedLocker prevents certificate jobs from sharing an advisory-lock key
// across panels, even when the same SNI exists in both.
type ScopedLocker struct {
	panelID string
	next    Locker
}

func NewScopedLocker(panelID string, next Locker) (*ScopedLocker, error) {
	panelID = strings.TrimSpace(panelID)
	if panelID == "" || next == nil {
		return nil, ErrInvalidInput
	}
	return &ScopedLocker{panelID: panelID, next: next}, nil
}

func (l *ScopedLocker) Lock(ctx context.Context, key string) (func(), error) {
	// PostgreSQL text parameters reject NUL bytes. Use a length-prefixed key so
	// panel/SNI pairs remain unambiguous without sending binary data through the
	// advisory-lock repository contract.
	scopedKey := strconv.Itoa(len(l.panelID)) + ":" + l.panelID + ":" + key
	return l.next.Lock(ctx, scopedKey)
}

var _ Locker = (*ScopedLocker)(nil)
