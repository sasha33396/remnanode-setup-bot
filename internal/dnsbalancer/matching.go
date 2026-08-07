package dnsbalancer

import (
	"errors"
	"fmt"
	"strings"
)

var (
	// ErrNotFound represents both an HTTP 404 and a missing local FQDN match.
	ErrNotFound = errors.New("DNS-balancer resource not found")
	// ErrAmbiguousZone indicates duplicate configuration producing one FQDN.
	ErrAmbiguousZone = errors.New("ambiguous DNS-balancer zone")
	// ErrInvalidInput indicates invalid caller input.
	ErrInvalidInput = errors.New("invalid DNS-balancer client input")
)

// LocateZone matches fqdn against complete names constructed from the actual
// domain/zone configuration. It never guesses a parent domain by splitting the
// requested FQDN.
func LocateZone(domains []Domain, fqdn string) (ZoneMatch, error) {
	target := normalizeFQDN(fqdn)
	if target == "" {
		return ZoneMatch{}, fmt.Errorf("FQDN: %w", ErrInvalidInput)
	}

	var match *ZoneMatch
	for _, domain := range domains {
		for _, zone := range domain.Zones {
			candidate := zoneFQDN(domain.Name, zone.Name)
			if candidate != target {
				continue
			}
			if match != nil {
				return ZoneMatch{}, fmt.Errorf("FQDN %q: %w", fqdn, ErrAmbiguousZone)
			}
			current := ZoneMatch{
				Domain:   domain.Name,
				ZoneName: zone.Name,
				FQDN:     candidate,
				Zone:     zone,
			}
			match = &current
		}
	}
	if match == nil {
		return ZoneMatch{}, fmt.Errorf("FQDN %q: %w", fqdn, ErrNotFound)
	}
	return *match, nil
}

func zoneFQDN(domain, zone string) string {
	domain = normalizeFQDN(domain)
	zone = normalizeFQDN(zone)
	if zone == "@" {
		return domain
	}
	if domain == "" || zone == "" {
		return ""
	}
	return zone + "." + domain
}

func normalizeFQDN(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}
