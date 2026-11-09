package orchestrator

import (
	"net/netip"
	"testing"

	"remnanode-setup-bot/internal/abuse"
)

func TestSplitCompleteLogLinesCarriesPartialLineAcrossChunks(t *testing.T) {
	complete, partial := splitCompleteLogLines("old ", "line\nsecond", " line\nthird")
	if complete != "old line\nsecond line" || partial != "third" {
		t.Fatalf("complete=%q partial=%q", complete, partial)
	}
}

func TestSplitCompleteLogLinesSupportsRotationChunksWithoutDuplication(t *testing.T) {
	complete, partial := splitCompleteLogLines("", "rotated-1\nrotated-2\n", "current-1\ncurrent-part")
	if complete != "rotated-1\nrotated-2\ncurrent-1" || partial != "current-part" {
		t.Fatalf("complete=%q partial=%q", complete, partial)
	}
}

func TestEnrichUnknownEventsRefreshesHistoricalRowsOncePerIP(t *testing.T) {
	ip := netip.MustParseAddr("91.107.117.52")
	events := []abuse.Event{{ClientIP: ip, Provider: "UNKNOWN", Category: "UNKNOWN"}, {ClientIP: ip, Provider: "UNKNOWN", Category: "UNKNOWN"}}
	calls := 0
	intel := enrichUnknownEvents(events, func(got netip.Addr) abuse.Intelligence {
		calls++
		if got != ip {
			t.Fatalf("lookup IP = %s", got)
		}
		return abuse.Intelligence{ASN: 207235, Provider: "GURZUF-21-VEK", Category: "UNKNOWN"}
	})
	if calls != 1 || intel[ip].ASN != 207235 || events[0].ASN != 207235 || events[1].Provider != "GURZUF-21-VEK" {
		t.Fatalf("calls=%d intel=%#v events=%#v", calls, intel, events)
	}
}
