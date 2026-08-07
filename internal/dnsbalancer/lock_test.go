package dnsbalancer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryZoneLockerHonorsContext(t *testing.T) {
	locker := NewMemoryZoneLocker()
	unlock, err := locker.Lock(context.Background(), "de.example.com")
	if err != nil {
		t.Fatalf("first Lock() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = locker.Lock(ctx, "de.example.com")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Lock() error = %v, want deadline exceeded", err)
	}

	unlock()
	unlockAgain, err := locker.Lock(context.Background(), "de.example.com")
	if err != nil {
		t.Fatalf("Lock() after unlock error = %v", err)
	}
	unlockAgain()
}
