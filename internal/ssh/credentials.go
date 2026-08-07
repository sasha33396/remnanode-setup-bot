// Package ssh provides verified SSH connectivity and bounded command execution.
package ssh

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
)

// InitialCredentials contains the temporary password only in memory. Its
// String and GoString forms are always redacted.
type InitialCredentials struct {
	Address  netip.Addr
	Username string

	password *passwordSecret
}

type passwordSecret struct {
	mu    sync.Mutex
	value []byte
}

// NewInitialCredentials copies temporaryPassword into private mutable memory.
// Call Clear as soon as password authentication is no longer needed.
func NewInitialCredentials(address netip.Addr, username string, temporaryPassword []byte) *InitialCredentials {
	return &InitialCredentials{
		Address:  address,
		Username: username,
		password: &passwordSecret{value: append([]byte(nil), temporaryPassword...)},
	}
}

// Clear overwrites the client's in-memory password copy.
func (c *InitialCredentials) Clear() {
	if c.password == nil {
		return
	}
	c.password.clear()
}

func (c *InitialCredentials) passwordString() string {
	if c.password == nil {
		return ""
	}
	return c.password.reveal()
}

func (c *InitialCredentials) String() string {
	return fmt.Sprintf("InitialCredentials{Address:%s Username:%s Password:[REDACTED]}", c.Address, c.Username)
}

func (c *InitialCredentials) GoString() string { return c.String() }

// LogValue prevents structured loggers from reflecting over credential fields.
func (c *InitialCredentials) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("address", c.Address.String()),
		slog.String("username", c.Username),
		slog.String("password", "[REDACTED]"),
	)
}

func (s *passwordSecret) reveal() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.value)
}

func (s *passwordSecret) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for index := range s.value {
		s.value[index] = 0
	}
	s.value = nil
}

func (s *passwordSecret) String() string   { return "[REDACTED]" }
func (s *passwordSecret) GoString() string { return "[REDACTED]" }
