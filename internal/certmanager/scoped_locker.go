package certmanager

import (
	"context"
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
	return l.next.Lock(ctx, l.panelID+"\x00"+key)
}

var _ Locker = (*ScopedLocker)(nil)
