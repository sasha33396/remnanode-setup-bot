package certmanager

import (
	"context"
	"sync"
)

// MemoryLocker provides context-aware per-SNI locking for a single service
// instance. PostgreSQLLocker extends this across multiple instances.
type MemoryLocker struct {
	mu      sync.Mutex
	entries map[string]*lockEntry
}

type lockEntry struct {
	token chan struct{}
	refs  int
}

func NewMemoryLocker() *MemoryLocker {
	return &MemoryLocker{entries: make(map[string]*lockEntry)}
}

func (l *MemoryLocker) Lock(ctx context.Context, key string) (func(), error) {
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

func (l *MemoryLocker) release(key string, entry *lockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.refs--
	if entry.refs == 0 && l.entries[key] == entry {
		delete(l.entries, key)
	}
}

var _ Locker = (*MemoryLocker)(nil)
