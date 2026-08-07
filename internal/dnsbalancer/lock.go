package dnsbalancer

import (
	"context"
	"sync"
)

// ZoneLocker serializes read-modify-write operations for one FQDN. A future
// PostgreSQL advisory-lock implementation can satisfy this interface.
type ZoneLocker interface {
	Lock(context.Context, string) (unlock func(), err error)
}

// MemoryZoneLocker is a process-local keyed mutex with context-aware waiting.
type MemoryZoneLocker struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	token chan struct{}
	refs  int
}

// NewMemoryZoneLocker creates an in-memory keyed locker.
func NewMemoryZoneLocker() *MemoryZoneLocker {
	return &MemoryZoneLocker{entries: make(map[string]*lockEntry)}
}

// Lock acquires the lock for key or returns when ctx is cancelled.
func (l *MemoryZoneLocker) Lock(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	entry := l.entries[key]
	if entry == nil {
		entry = &lockEntry{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	select {
	case <-entry.token:
		var once sync.Once
		return func() {
			once.Do(func() {
				entry.token <- struct{}{}
				l.release(key, entry)
			})
		}, nil
	case <-ctx.Done():
		l.release(key, entry)
		return nil, ctx.Err()
	}
}

func (l *MemoryZoneLocker) release(key string, entry *lockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && l.entries[key] == entry {
		delete(l.entries, key)
	}
}
