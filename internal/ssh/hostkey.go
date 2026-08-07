package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

var ErrHostKeyChanged = errors.New("SSH host key changed")

// HostKeyStore associates one trusted fingerprint with a deployment. The
// StoreIfAbsent operation must be atomic for concurrent first connections.
type HostKeyStore interface {
	Get(context.Context, string) (fingerprint string, found bool, err error)
	StoreIfAbsent(context.Context, string, string) (trustedFingerprint string, stored bool, err error)
}

// MemoryHostKeyStore is process-local storage useful until persistence is
// wired through the repository layer.
type MemoryHostKeyStore struct {
	mu           sync.RWMutex
	fingerprints map[string]string
}

func NewMemoryHostKeyStore() *MemoryHostKeyStore {
	return &MemoryHostKeyStore{fingerprints: make(map[string]string)}
}

func (s *MemoryHostKeyStore) Get(ctx context.Context, deploymentID string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	fingerprint, found := s.fingerprints[deploymentID]
	return fingerprint, found, nil
}

func (s *MemoryHostKeyStore) StoreIfAbsent(ctx context.Context, deploymentID, fingerprint string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if trusted, found := s.fingerprints[deploymentID]; found {
		return trusted, false, nil
	}
	s.fingerprints[deploymentID] = fingerprint
	return fingerprint, true, nil
}

// HostKeyChangedError reports fingerprints, which are public identifiers, but
// never credentials.
type HostKeyChangedError struct {
	Expected string
	Actual   string
}

func (e *HostKeyChangedError) Error() string {
	return fmt.Sprintf("SSH host key changed: expected %s, received %s", e.Expected, e.Actual)
}

func (e *HostKeyChangedError) Unwrap() error { return ErrHostKeyChanged }

type hostKeySession struct {
	ctx          context.Context
	deploymentID string
	store        HostKeyStore

	mu        sync.Mutex
	candidate string
}

func newHostKeySession(ctx context.Context, deploymentID string, store HostKeyStore) *hostKeySession {
	return &hostKeySession{ctx: ctx, deploymentID: deploymentID, store: store}
}

// callback implements TOFU with explicit verification. A first key is
// provisionally accepted and committed only after authentication succeeds.
func (s *hostKeySession) callback(_ string, _ net.Addr, key gossh.PublicKey) error {
	fingerprint := gossh.FingerprintSHA256(key)
	trusted, found, err := s.store.Get(s.ctx, s.deploymentID)
	if err != nil {
		return fmt.Errorf("load trusted SSH host key: %w", err)
	}
	if found {
		if trusted != fingerprint {
			return &HostKeyChangedError{Expected: trusted, Actual: fingerprint}
		}
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candidate != "" && s.candidate != fingerprint {
		return &HostKeyChangedError{Expected: s.candidate, Actual: fingerprint}
	}
	s.candidate = fingerprint
	return nil
}

func (s *hostKeySession) commit() error {
	s.mu.Lock()
	candidate := s.candidate
	s.mu.Unlock()
	if candidate == "" {
		return errors.New("SSH server did not present a host key")
	}
	trusted, _, err := s.store.StoreIfAbsent(s.ctx, s.deploymentID, candidate)
	if err != nil {
		return fmt.Errorf("store trusted SSH host key: %w", err)
	}
	if trusted != candidate {
		return &HostKeyChangedError{Expected: trusted, Actual: candidate}
	}
	return nil
}
