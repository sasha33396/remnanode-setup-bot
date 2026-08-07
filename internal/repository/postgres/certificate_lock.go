package postgres

import (
	"context"
	"sync"
	"time"

	"remnanode-setup-bot/internal/certmanager"
)

// Lock serializes ACME work for one SNI across service instances by holding a
// PostgreSQL advisory lock on a dedicated pooled connection.
func (r *Repository) Lock(ctx context.Context, sni string) (func(), error) {
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended(lower(btrim($1)), 0))`, sni); err != nil {
		connection.Release()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = connection.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended(lower(btrim($1)), 0))`, sni)
			connection.Release()
		})
	}, nil
}

var _ certmanager.Locker = (*Repository)(nil)
