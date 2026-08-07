package dnsbalancer

import (
	"errors"
	"testing"
)

func TestLocateZoneUsesActualConfiguration(t *testing.T) {
	domains := []Domain{
		{Name: "example.com", Zones: []Zone{{Name: "de"}, {Name: "@"}}},
		{Name: "other.example.net", Zones: []Zone{{Name: "edge"}}},
	}

	tests := []struct {
		fqdn       string
		wantDomain string
		wantZone   string
	}{
		{"de.example.com", "example.com", "de"},
		{"example.com", "example.com", "@"},
		{"EDGE.OTHER.EXAMPLE.NET.", "other.example.net", "edge"},
	}
	for _, test := range tests {
		match, err := LocateZone(domains, test.fqdn)
		if err != nil {
			t.Fatalf("LocateZone(%q) error = %v", test.fqdn, err)
		}
		if match.Domain != test.wantDomain || match.ZoneName != test.wantZone {
			t.Errorf("LocateZone(%q) = domain %q zone %q", test.fqdn, match.Domain, match.ZoneName)
		}
	}

	if _, err := LocateZone(domains, "deep.de.example.com"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LocateZone(unconfigured) error = %v, want ErrNotFound", err)
	}
}

func TestLocateZoneRejectsAmbiguousConfiguration(t *testing.T) {
	domains := []Domain{
		{Name: "example.com", Zones: []Zone{{Name: "de"}}},
		{Name: "EXAMPLE.COM", Zones: []Zone{{Name: "DE"}}},
	}
	_, err := LocateZone(domains, "de.example.com")
	if !errors.Is(err, ErrAmbiguousZone) {
		t.Fatalf("LocateZone() error = %v, want ErrAmbiguousZone", err)
	}
}
