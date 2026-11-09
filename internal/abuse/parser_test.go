package abuse

import (
	"net/netip"
	"testing"
	"time"
)

func TestParseAccessLine(t *testing.T) {
	event, err := ParseAccessLine("2026/11/09 14:37:23.250624 from 31.76.11.151:64206 accepted tcp:46.255.231.70:587 [VLESS_TCP_REALITY_AT >> DIRECT] email: 44741", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if event.ClientIP.String() != "31.76.11.151" || event.ClientPort != 64206 || event.DestinationPort != 587 || event.Inbound != "VLESS_TCP_REALITY_AT" || event.Outbound != "DIRECT" || event.UserID != "44741" {
		t.Fatalf("unexpected event: %#v", event)
	}
	if event.OccurredAt.Location() != time.UTC {
		t.Fatalf("timestamp location = %v", event.OccurredAt.Location())
	}
}

func TestParseBlockRoutes(t *testing.T) {
	for _, route := range []string{"IN -> BLOCK", "IN >> BLOCK"} {
		line := "2026/11/09 14:37:23 from 31.76.11.151:1234 accepted tcp:mail.example:25 [" + route + "] email: 7"
		event, err := ParseAccessLine(line, time.UTC)
		if err != nil || !event.Blocked {
			t.Fatalf("route %q: event=%#v err=%v", route, event, err)
		}
	}
}

func TestAssessmentDoesNotFlagNormalIMAP(t *testing.T) {
	now := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC)
	event := Event{PanelID: "a", UserID: "42", OccurredAt: now.Add(-time.Minute), ClientIP: netip.MustParseAddr("192.0.2.1"), DestinationPort: 993}
	got := Assess("a", "42", []Event{event}, nil, nil, now, DefaultThresholds())
	if got.Risk != RiskLow || got.Score != 0 {
		t.Fatalf("assessment = %#v", got)
	}
}

func TestAssessmentScopesSameUserToPanel(t *testing.T) {
	now := time.Now().UTC()
	events := make([]Event, 0, 60)
	for i := 0; i < 60; i++ {
		events = append(events, Event{PanelID: "other", UserID: "42", OccurredAt: now, ClientIP: netip.MustParseAddr("192.0.2.1"), DestinationPort: 25})
	}
	if got := Assess("main", "42", events, nil, nil, now, DefaultThresholds()); got.Risk != RiskLow {
		t.Fatalf("cross-panel events leaked: %#v", got)
	}
}

func TestMassSMTPCreatesHighRisk(t *testing.T) {
	now := time.Now().UTC()
	events := make([]Event, 0, 60)
	for i := 0; i < 60; i++ {
		events = append(events, Event{PanelID: "main", UserID: "42", OccurredAt: now.Add(-time.Minute), ClientIP: netip.MustParseAddr("192.0.2.1"), DestinationPort: 25})
	}
	if got := Assess("main", "42", events, nil, nil, now, DefaultThresholds()); got.Risk != RiskHigh {
		t.Fatalf("assessment = %#v", got)
	}
}

func TestAssessmentUsesRemnawaveIPLastSeenButKeepsMailCountsFromLogs(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	event := Event{PanelID: "main", UserID: "42", OccurredAt: now.Add(-time.Minute), ClientIP: netip.MustParseAddr("192.0.2.1"), DestinationPort: 25}
	observed := map[netip.Addr]time.Time{
		netip.MustParseAddr("8.8.8.8"): now.Add(-10 * time.Minute),
		netip.MustParseAddr("1.1.1.1"): now.Add(-2 * time.Hour),
	}
	got := Assess("main", "42", []Event{event}, nil, observed, now, DefaultThresholds())
	if !got.IPsFromAPI || got.UniqueIPs1H != 1 || got.UniqueIPs24H != 2 || got.SMTP25Attempts10M != 1 {
		t.Fatalf("assessment = %#v", got)
	}
}
