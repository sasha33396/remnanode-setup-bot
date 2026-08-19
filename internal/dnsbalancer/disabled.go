package dnsbalancer

import (
	"context"
	"net/netip"
)

// Disabled implements API for a panel that deliberately has no DNS
// balancing. Orchestration must explicitly skip DNS effects for such panels.
type Disabled struct{}

func (Disabled) GetDomains(context.Context) ([]Domain, error)        { return []Domain{}, nil }
func (Disabled) FindZone(context.Context, string) (ZoneMatch, error) { return ZoneMatch{}, ErrNotFound }
func (Disabled) FindZonesByIP(context.Context, netip.Addr) ([]ZoneMatch, error) {
	return []ZoneMatch{}, nil
}
func (Disabled) AddIP(context.Context, string, netip.Addr) (AddIPResult, error) {
	return AddIPResult{}, ErrInvalidInput
}
func (Disabled) ReplaceIP(context.Context, string, netip.Addr, netip.Addr) (ReplaceIPResult, error) {
	return ReplaceIPResult{}, ErrInvalidInput
}

var _ API = Disabled{}
