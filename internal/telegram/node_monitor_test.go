package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNodeMonitorAlertsAllUsersAfterConfirmationAndReportsRecovery(t *testing.T) {
	baseApplication := &fakeApplication{
		panels: []Panel{{ID: "hit", Name: "Hit"}},
		nodes: []NodeSummary{
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000001", Name: "de-low", Address: "203.0.113.1", Connected: true, OnlineKnown: true, Online: 50},
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000002", Name: "de-good-1", Address: "203.0.113.2", Connected: true, OnlineKnown: true, Online: 320},
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000003", Name: "de-good-2", Address: "203.0.113.3", Connected: true, OnlineKnown: true, Online: 360},
		},
	}
	application := &fakeNodeIPApplication{fakeApplication: baseApplication}
	messenger := &fakeMessenger{}
	monitor, err := NewNodeMonitor([]int64{101, 202}, application, messenger, time.Minute, 15*time.Minute, 2, DefaultNodePolicy())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	monitor.now = func() time.Time { return now }

	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 0 {
		t.Fatalf("alerts after first sample = %d, want 0", len(got))
	}
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	alerts := sentRecords(messenger)
	if len(alerts) != 2 || alerts[0].message.ChatID != 101 || alerts[1].message.ChatID != 202 {
		t.Fatalf("alerts = %#v", alerts)
	}
	for _, alert := range alerts {
		if !strings.Contains(alert.text, "Критически низкий онлайн") || !strings.Contains(alert.text, "Онлайн: 50") || alert.keyboard.Inline[0][0].CallbackData != "nodes:ip:00000000-0000-0000-0000-000000000001" || alert.keyboard.Inline[1][0].CallbackData != "nodes:o:00000000-0000-0000-0000-000000000001" {
			t.Fatalf("alert = %#v", alert)
		}
	}
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 2 {
		t.Fatalf("duplicate alerts sent: %d", len(got))
	}
	now = now.Add(14 * time.Minute)
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 2 {
		t.Fatalf("alert repeated before 15 minutes: %d", len(got))
	}
	now = now.Add(time.Minute)
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 4 {
		t.Fatalf("15-minute reminder count = %d, want 4", len(got))
	}

	application.mu.Lock()
	application.nodes[0].Connected = false
	application.nodes[0].OnlineKnown = false
	application.mu.Unlock()
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	application.mu.Lock()
	application.nodes[0].Connected = true
	application.nodes[0].OnlineKnown = true
	application.mu.Unlock()
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 4 {
		t.Fatalf("telemetry gap caused duplicate alert: %d", len(got))
	}

	application.mu.Lock()
	application.nodes[0].Online = 280
	application.mu.Unlock()
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := sentRecords(messenger)
	if len(records) != 6 || !strings.Contains(records[4].text, "восстановился") || !strings.Contains(records[5].text, "восстановился") {
		t.Fatalf("recovery records = %#v", records)
	}
}

func TestNodeMonitorIgnoresDisconnectedAndDisabledNodes(t *testing.T) {
	application := &fakeApplication{
		panels: []Panel{{ID: "hit", Name: "Hit"}},
		nodes: []NodeSummary{
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000001", Name: "lost", OnlineKnown: true, Online: 0},
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000002", Name: "off", Disabled: true, OnlineKnown: true, Online: 0},
		},
	}
	messenger := &fakeMessenger{}
	monitor, err := NewNodeMonitor([]int64{101}, application, messenger, time.Minute, 15*time.Minute, 1, DefaultNodePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 0 {
		t.Fatalf("ignored nodes produced %d alerts", len(got))
	}
}

func sentRecords(messenger *fakeMessenger) []sentRecord {
	messenger.mu.Lock()
	defer messenger.mu.Unlock()
	return append([]sentRecord(nil), messenger.sent...)
}
