package abuse

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalIntelligenceUsesLongestPrefixAndASNOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "intel.csv")
	if err := os.WriteFile(path, []byte("203.0.113.0/24,AS64500,Wide,RESIDENTIAL\n203.0.113.128/25,AS64501,Narrow,HOSTING\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := LoadLocalIntelligence(path, map[uint32]string{64501: "PROXY"})
	if err != nil {
		t.Fatal(err)
	}
	got := db.Lookup(netip.MustParseAddr("203.0.113.200"))
	if got.ASN != 64501 || got.Provider != "Narrow" || got.Category != "PROXY" {
		t.Fatalf("lookup = %#v", got)
	}
	unknown := db.Lookup(netip.MustParseAddr("192.0.2.1"))
	if unknown.Provider != "UNKNOWN" || unknown.Category != "UNKNOWN" {
		t.Fatalf("unknown lookup = %#v", unknown)
	}
}
