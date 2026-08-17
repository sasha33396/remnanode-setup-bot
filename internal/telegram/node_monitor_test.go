package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNodeMonitorAlertsAllUsersAfterConfirmationAndReportsRecovery(t *testing.T) {
	application := &fakeApplication{
		panels: []Panel{{ID: "hit", Name: "Hit"}},
		nodes: []NodeSummary{
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000001", Name: "de-low", Address: "203.0.113.1", Connected: true, OnlineKnown: true, Online: 55},
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000002", Name: "de-good-1", Address: "203.0.113.2", Connected: true, OnlineKnown: true, Online: 320},
			{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000003", Name: "de-good-2", Address: "203.0.113.3", Connected: true, OnlineKnown: true, Online: 360},
		},
	}
	messenger := &fakeMessenger{}
	monitor, err := NewNodeMonitor([]int64{101, 202}, application, messenger, time.Minute, 2, DefaultNodePolicy())
	if err != nil {
		t.Fatal(err)
	}

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
		if !strings.Contains(alert.text, "Критически низкий онлайн") || !strings.Contains(alert.text, "Онлайн: 55") || alert.keyboard.Inline[0][0].CallbackData != "nodes:o:00000000-0000-0000-0000-000000000001" {
			t.Fatalf("alert = %#v", alert)
		}
	}
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sentRecords(messenger); len(got) != 2 {
		t.Fatalf("duplicate alerts sent: %d", len(got))
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
	if got := sentRecords(messenger); len(got) != 2 {
		t.Fatalf("telemetry gap caused duplicate alert: %d", len(got))
	}

	application.mu.Lock()
	application.nodes[0].Online = 280
	application.mu.Unlock()
	if err := monitor.sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	records := sentRecords(messenger)
	if len(records) != 4 || !strings.Contains(records[2].text, "восстановился") || !strings.Contains(records[3].text, "восстановился") {
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
	monitor, err := NewNodeMonitor([]int64{101}, application, messenger, time.Minute, 1, DefaultNodePolicy())
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
