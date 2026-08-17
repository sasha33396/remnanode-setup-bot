package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testAllowedUser = int64(42)
	testChatID      = int64(420)
)

func TestControllerRejectsUnauthorizedUsers(t *testing.T) {
	application := &fakeApplication{}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })

	if err := controller.Handle(context.Background(), Update{Message: &Message{ID: 1, ChatID: 990, FromUserID: 99, Text: MenuAddNode}}); err != nil {
		t.Fatalf("Handle() error = %v", err)
	}
	if application.totalCalls() != 0 {
		t.Fatalf("unauthorized update reached application: %d calls", application.totalCalls())
	}
	if got := messenger.lastSent().text; got != "Access denied." {
		t.Fatalf("unauthorized response = %q", got)
	}

	if err := controller.Handle(context.Background(), Update{CallbackQuery: &CallbackQuery{ID: "unauthorized", FromUserID: 99, Data: "add:deploy:nonce"}}); err != nil {
		t.Fatalf("Handle(callback) error = %v", err)
	}
	if got := messenger.lastAnswer().text; got != "Access denied." {
		t.Fatalf("unauthorized callback answer = %q", got)
	}
}

func TestTelegramButtonsUseRussianLabels(t *testing.T) {
	menu := mainKeyboard()
	wantReply := [][]string{{"➕ Добавить ноду", "🔄 Сменить IP"}, {"📡 Ноды", "📜 Развёртывания"}}
	if fmt.Sprint(menu.Reply) != fmt.Sprint(wantReply) {
		t.Fatalf("main keyboard = %#v, want %#v", menu.Reply, wantReply)
	}
	confirmation := confirmationKeyboard("nonce")
	if confirmation.Inline[0][0].Text != "🚀 Развернуть" || confirmation.Inline[1][0].Text != "❌ Отмена" {
		t.Fatalf("confirmation keyboard = %#v", confirmation)
	}
}

func TestNodesAreSeparatedByPanelAndPriority(t *testing.T) {
	panels := []Panel{{ID: "hit", Name: "Hit"}, {ID: "horda", Name: "Horda"}}
	nodes := []NodeSummary{
		{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000001", Name: "de-low", Address: "203.0.113.1", Connected: true, OnlineKnown: true, Online: 55},
		{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000002", Name: "de-good-1", Address: "203.0.113.2", Connected: true, OnlineKnown: true, Online: 320},
		{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000003", Name: "de-good-2", Address: "203.0.113.3", Connected: true, OnlineKnown: true, Online: 360},
		{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000004", Name: "de-off", Address: "203.0.113.4", Disabled: true},
		{PanelID: "hit", PanelName: "Hit", UUID: "00000000-0000-0000-0000-000000000005", Name: "de-disconnected", Address: "203.0.113.5"},
		{PanelID: "horda", PanelName: "Horda", UUID: "00000000-0000-0000-0000-000000000006", Name: "nl-good", Address: "203.0.113.6", Connected: true, OnlineKnown: true, Online: 250},
	}
	application := &fakeApplication{panels: panels, nodes: nodes}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })

	handleMessage(t, controller, 1, MenuNodes)
	first := messenger.lastSent()
	if !strings.Contains(first.text, "Выберите Remnawave-панель") || len(first.keyboard.Inline) != 2 {
		t.Fatalf("panel picker = %q %#v", first.text, first.keyboard)
	}
	if got := first.keyboard.Inline[0][0]; got.Text != "Hit — 5" || got.CallbackData != "nodes:p:0" {
		t.Fatalf("Hit button = %#v", got)
	}
	handleCallback(t, controller, "nodes-panel", "nodes:p:0", first.message)
	edits := messenger.editsSnapshot()
	panel := edits[len(edits)-1]
	if !strings.Contains(panel.text, "Критический порог: менее 128") || !strings.Contains(panel.text, "не участвуют в тревогах: 1") {
		t.Fatalf("panel summary = %q", panel.text)
	}
	wantButtons := []string{"🚨 Критический онлайн — 1", "⏸ Отключённые — 1", "🟢 Активные / стабильные — 2"}
	for index, want := range wantButtons {
		if got := panel.keyboard.Inline[index][0].Text; got != want {
			t.Fatalf("group button %d = %q, want %q", index, got, want)
		}
	}

	handleCallback(t, controller, "nodes-critical", "nodes:g:0:c:0", first.message)
	edits = messenger.editsSnapshot()
	critical := edits[len(edits)-1]
	if !strings.Contains(critical.text, "Критический онлайн — Hit") || critical.keyboard.Inline[0][0].Text != "🚨 de-low — онлайн 55" {
		t.Fatalf("critical list = %q %#v", critical.text, critical.keyboard)
	}

	handleCallback(t, controller, "nodes-card", critical.keyboard.Inline[0][0].CallbackData, first.message)
	edits = messenger.editsSnapshot()
	card := edits[len(edits)-1]
	if !strings.Contains(card.text, "Нода: de-low") || !strings.Contains(card.text, "Рекомендация") || strings.Contains(card.text, nodes[0].UUID) {
		t.Fatalf("node card = %q", card.text)
	}
}

func TestDisconnectedNodeIsIgnoredByLowOnlineClassification(t *testing.T) {
	snapshots := classifyNodePanels(
		[]Panel{{ID: "hit", Name: "Hit"}},
		[]NodeSummary{{PanelID: "hit", UUID: "00000000-0000-0000-0000-000000000001", Name: "lost", OnlineKnown: true, Online: 0}},
		DefaultNodePolicy(),
	)
	if len(snapshots) != 1 || len(snapshots[0].Critical) != 0 || len(snapshots[0].Ignored) != 1 {
		t.Fatalf("classification = %#v", snapshots)
	}
}

func TestCertificateBootstrapRequiresExplicitConfirmation(t *testing.T) {
	application := &fakeRecoveryApplication{fakeApplication: &fakeApplication{}, bootstrapResult: "Certificate staged-version activated"}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })

	handleMessage(t, controller, 1, "/bootstrap_certificate edge.example.com")
	if application.bootstrapCalls != 0 || !strings.Contains(messenger.lastSent().text, "CONFIRM") {
		t.Fatalf("bootstrap without confirmation was not rejected: calls=%d response=%q", application.bootstrapCalls, messenger.lastSent().text)
	}
	handleMessage(t, controller, 2, "/bootstrap_certificate edge.example.com CONFIRM")
	if application.bootstrapCalls != 1 || application.bootstrapSNI != "edge.example.com" || application.bootstrapOperator != testAllowedUser {
		t.Fatalf("bootstrap call = %d, %q, %d", application.bootstrapCalls, application.bootstrapSNI, application.bootstrapOperator)
	}
	if !strings.Contains(messenger.lastSent().text, "activated") {
		t.Fatalf("bootstrap response = %q", messenger.lastSent().text)
	}
	application.bootstrapResult = "No valid staged certificate is available"
	application.bootstrapErr = errors.New("bootstrap failed")
	handleMessage(t, controller, 3, "/bootstrap_certificate edge.example.com CONFIRM")
	if !strings.Contains(messenger.lastSent().text, application.bootstrapResult) {
		t.Fatalf("safe bootstrap failure = %q", messenger.lastSent().text)
	}
}

func TestReplaceIPButtonWizardSupportsLegacyNode(t *testing.T) {
	application := &fakeNodeIPApplication{
		fakeApplication: &fakeApplication{},
		target:          NodeIPChangeTarget{UUID: "internal-uuid", Name: "legacy-node", Address: netip.MustParseAddr("8.8.8.8"), Connected: true, DNSZones: []string{"edge.example.com"}},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "legacy-node")
	card := messenger.lastSent()
	if !strings.Contains(card.text, "legacy-node") || !strings.Contains(card.text, "8.8.8.8") || !strings.Contains(card.text, "legacy") {
		t.Fatalf("card = %q", card.text)
	}
	if strings.Contains(card.text, "internal-uuid") || len(card.keyboard.Inline) != 1 {
		t.Fatalf("unsafe card = %#v", card)
	}
	handleCallback(t, controller, "change", card.keyboard.Inline[0][0].CallbackData, card.message)
	handleMessage(t, controller, 3, "1.1.1.1")
	if application.replaceCalls != 1 || application.input.NodeUUID != "internal-uuid" || application.input.ExpectedIP.String() != "8.8.8.8" || application.input.NewIP.String() != "1.1.1.1" {
		t.Fatalf("replace input = %#v", application.input)
	}
}

func TestNodeCardStartsPrefilledIPChange(t *testing.T) {
	uuid := "00000000-0000-0000-0000-000000000001"
	application := &fakeNodeIPApplication{
		fakeApplication: &fakeApplication{
			panels: []Panel{{ID: "hit", Name: "Hit", DNSEnabled: true}},
			nodes:  []NodeSummary{{PanelID: "hit", PanelName: "Hit", UUID: uuid, Name: "de-low", Address: "8.8.8.8", Connected: true, OnlineKnown: true, Online: 55}},
		},
		target: NodeIPChangeTarget{PanelName: "Hit", DNSEnabled: true, UUID: uuid, Name: "de-low", Address: netip.MustParseAddr("8.8.8.8"), Connected: true, DNSZones: []string{"de.example.com"}},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })

	if err := controller.showNodeCard(context.Background(), testChatID, 0, uuid); err != nil {
		t.Fatal(err)
	}
	card := messenger.lastSent()
	if got := card.keyboard.Inline[0][0]; got.Text != "🔄 Изменить IP ноды" || got.CallbackData != "nodes:ip:"+uuid {
		t.Fatalf("IP change card button = %#v", got)
	}
	handleCallback(t, controller, "node-ip", card.keyboard.Inline[0][0].CallbackData, card.message)
	if application.findPanel != "hit" || application.findQuery != "de-low" {
		t.Fatalf("prefilled lookup = panel %q, query %q", application.findPanel, application.findQuery)
	}
	edits := messenger.editsSnapshot()
	confirmation := edits[len(edits)-1]
	if !strings.Contains(confirmation.text, "de-low") || !strings.Contains(confirmation.text, "8.8.8.8") || confirmation.keyboard.Inline[0][0].Text != "🔄 Сменить" {
		t.Fatalf("IP confirmation = %#v", confirmation)
	}
	handleCallback(t, controller, "node-ip-confirm", confirmation.keyboard.Inline[0][0].CallbackData, card.message)
	handleMessage(t, controller, 2, "1.1.1.1")
	if application.replaceCalls != 1 || application.input.PanelID != "hit" || application.input.NodeUUID != uuid || application.input.ExpectedIP.String() != "8.8.8.8" || application.input.NewIP.String() != "1.1.1.1" {
		t.Fatalf("prefilled replace input = %#v", application.input)
	}
}

func TestDNSSyncWizardUsesRemnawaveIPAndRequiresConfirmation(t *testing.T) {
	application := &fakeDNSSyncApplication{
		fakeApplication: &fakeApplication{},
		target: NodeDNSSyncTarget{
			PanelName:       "Hit",
			UUID:            "internal-node-uuid",
			Name:            "de-new-0",
			Address:         netip.MustParseAddr("177.1.202.161"),
			Connected:       true,
			Managed:         true,
			DNSZone:         "de-new.example.com",
			PreviousIP:      netip.MustParseAddr("85.155.125.5"),
			PreviousPresent: true,
			CanSync:         true,
			Note:            "Устаревший IP будет заменён актуальным IP из Remnawave",
		},
		result: NodeDNSSyncResult{NodeName: "de-new-0", Address: netip.MustParseAddr("177.1.202.161"), DNSZone: "de-new.example.com", Action: "REPLACED"},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })

	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	if len(mode.keyboard.Inline) != 1 || !strings.Contains(mode.keyboard.Inline[0][0].Text, "Remna → DNS") {
		t.Fatalf("DNS sync mode = %#v", mode.keyboard)
	}
	handleCallback(t, controller, "dns-mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "de-new-0")
	card := messenger.lastSent()
	if !strings.Contains(card.text, "Титульный IP (Remna): 177.1.202.161") || !strings.Contains(card.text, "de-new.example.com") || strings.Contains(card.text, "internal-node-uuid") {
		t.Fatalf("DNS sync card = %q", card.text)
	}
	if application.syncCalls != 0 || len(card.keyboard.Inline) != 1 {
		t.Fatalf("sync happened before confirmation: calls=%d keyboard=%#v", application.syncCalls, card.keyboard)
	}
	handleCallback(t, controller, "dns-confirm", card.keyboard.Inline[0][0].CallbackData, card.message)
	if application.syncCalls != 1 || application.input.NodeUUID != "internal-node-uuid" || application.input.ExpectedIP.String() != "177.1.202.161" {
		t.Fatalf("DNS sync input = %#v, calls=%d", application.input, application.syncCalls)
	}
	edits := messenger.editsSnapshot()
	if !strings.Contains(edits[len(edits)-1].text, "устаревший IP заменён актуальным") {
		t.Fatalf("DNS sync result = %q", edits[len(edits)-1].text)
	}
}

func TestDNSSyncCardAllowsLegacyZoneInferredFromProfile(t *testing.T) {
	text := renderDNSSyncTarget(NodeDNSSyncTarget{
		PanelName: "Hit",
		Name:      "de-10-cherry",
		Address:   netip.MustParseAddr("88.216.70.55"),
		DNSZone:   "de-modx.nodexphere.net",
		CanSync:   true,
		Note:      "Целевая зона определена по совпадению профиля и inbound ноды с Host",
	})
	if !strings.Contains(text, "legacy, зона определена по профилю + inbound") || !strings.Contains(text, "de-modx.nodexphere.net") || strings.Contains(text, "Автоматическая запись отключена") {
		t.Fatalf("inferred legacy card = %q", text)
	}
}

func TestDNSSyncAlwaysSendsResultAndReplaysCompletedCallback(t *testing.T) {
	application := &fakeDNSSyncApplication{
		fakeApplication: &fakeApplication{},
		target: NodeDNSSyncTarget{
			PanelName: "Hit", UUID: "node-uuid", Name: "node", Address: netip.MustParseAddr("88.216.70.55"),
			Managed: true, DNSZone: "de-modx.nodexphere.net", CanSync: true,
		},
		result: NodeDNSSyncResult{NodeName: "node", Address: netip.MustParseAddr("88.216.70.55"), DNSZone: "de-modx.nodexphere.net", Action: "ADDED"},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "dns-mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "node")
	card := messenger.lastSent()

	messenger.editErr = errors.New("Telegram rejected edit")
	handleCallback(t, controller, "dns-confirm", card.keyboard.Inline[0][0].CallbackData, card.message)
	if application.syncCalls != 1 || !strings.Contains(messenger.lastSent().text, "актуальный IP добавлен в DNS") {
		t.Fatalf("fallback result: calls=%d message=%q", application.syncCalls, messenger.lastSent().text)
	}
	handleCallback(t, controller, "dns-confirm-repeat", card.keyboard.Inline[0][0].CallbackData, card.message)
	if application.syncCalls != 1 || !strings.Contains(messenger.lastSent().text, "актуальный IP добавлен в DNS") || strings.Contains(messenger.lastSent().text, "expired") {
		t.Fatalf("replayed result: calls=%d message=%q", application.syncCalls, messenger.lastSent().text)
	}
}

func TestAddNodeRequiresPanelSelectionWhenMultipleConfigured(t *testing.T) {
	application := &fakeApplication{
		panels: []Panel{{ID: "europe", Name: "Europe", DNSEnabled: true}, {ID: "test", Name: "Test", DNSEnabled: false}},
		hosts:  []Host{{ID: "host-id", Remark: "Test Host", Address: "test.example.com"}},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuAddNode)
	picker := messenger.lastSent()
	if len(picker.keyboard.Inline) != 2 || !strings.Contains(picker.text, "панель") {
		t.Fatalf("panel picker = %#v", picker)
	}
	handleCallback(t, controller, "panel", picker.keyboard.Inline[1][0].CallbackData, picker.message)
	hosts := messenger.lastSent()
	if !strings.Contains(hosts.text, "Test") || len(hosts.keyboard.Inline) != 1 {
		t.Fatalf("host picker = %#v", hosts)
	}
}

func TestChangeIPIsScopedToSelectedPanel(t *testing.T) {
	application := &fakeNodeIPApplication{
		fakeApplication: &fakeApplication{panels: []Panel{{ID: "europe", Name: "Europe"}, {ID: "test", Name: "Test"}}},
		target:          NodeIPChangeTarget{PanelName: "Test", UUID: "node-uuid", Name: "legacy", Address: netip.MustParseAddr("8.8.8.8")},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	edits := messenger.editsSnapshot()
	picker := edits[len(edits)-1]
	handleCallback(t, controller, "panel", picker.keyboard.Inline[1][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "legacy")
	if application.findPanel != "test" {
		t.Fatalf("find panel = %q", application.findPanel)
	}
	card := messenger.lastSent()
	if !strings.Contains(card.text, "Панель: Test") {
		t.Fatalf("card = %q", card.text)
	}
}

func TestCherryIPWizardDeletesPasswordAndConfiguresServer(t *testing.T) {
	application := &fakeCherryIPApplication{fakeApplication: &fakeApplication{}}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "cherry-mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	edits := messenger.editsSnapshot()
	cherryMode := edits[len(edits)-1]
	handleCallback(t, controller, "without-node", cherryMode.keyboard.Inline[1][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "8.8.8.8")
	handleMessage(t, controller, 3, "1.1.1.1")
	password := &Message{ID: 4, ChatID: testChatID, FromUserID: testAllowedUser, Text: "root-secret"}
	if err := controller.Handle(context.Background(), Update{Message: password}); err != nil {
		t.Fatal(err)
	}
	controller.Wait()
	if password.Text != "" || !messenger.wasDeleted(testChatID, 4) {
		t.Fatal("Cherry root password was not removed from Telegram update")
	}
	if application.calls != 1 || application.input.ServerIP.String() != "8.8.8.8" || application.input.FloatingIP.String() != "1.1.1.1" || string(application.password) != "root-secret" {
		t.Fatalf("Cherry input = %#v password=%q calls=%d", application.input, application.password, application.calls)
	}
}

func TestCherryIPWizardUpdatesExistingRemnawaveNodeAndDNS(t *testing.T) {
	application := &fakeCherryNodeApplication{
		fakeApplication: &fakeApplication{},
		target: NodeIPChangeTarget{
			PanelName: "Hit",
			UUID:      "node-uuid",
			Name:      "legacy-node",
			Address:   netip.MustParseAddr("8.8.8.8"),
			DNSZones:  []string{"edge.example.com"},
		},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "cherry-mode", mode.keyboard.Inline[1][0].CallbackData, mode.message)
	edits := messenger.editsSnapshot()
	cherryMode := edits[len(edits)-1]
	handleCallback(t, controller, "with-node", cherryMode.keyboard.Inline[0][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "legacy-node")
	if got := messenger.lastSent().text; !strings.Contains(got, "legacy-node") || !strings.Contains(got, "обновит ноду и DNS") {
		t.Fatalf("existing-node prompt = %q", got)
	}
	handleMessage(t, controller, 3, "1.1.1.1")
	password := &Message{ID: 4, ChatID: testChatID, FromUserID: testAllowedUser, Text: "root-secret"}
	if err := controller.Handle(context.Background(), Update{Message: password}); err != nil {
		t.Fatal(err)
	}
	controller.Wait()
	if application.cherryCalls != 1 || application.cherryInput.ServerIP.String() != "8.8.8.8" || application.cherryInput.FloatingIP.String() != "1.1.1.1" {
		t.Fatalf("Cherry input = %#v calls=%d", application.cherryInput, application.cherryCalls)
	}
	if application.replaceCalls != 1 || application.replaceInput.PanelID != "default" || application.replaceInput.NodeUUID != "node-uuid" || application.replaceInput.ExpectedIP.String() != "8.8.8.8" || application.replaceInput.NewIP.String() != "1.1.1.1" {
		t.Fatalf("replace input = %#v calls=%d", application.replaceInput, application.replaceCalls)
	}
}

func TestRoyalIPWizardUpdatesServerNodeAndDNS(t *testing.T) {
	application := &fakeRoyalNodeApplication{
		fakeApplication: &fakeApplication{},
		target: NodeIPChangeTarget{
			PanelName: "Royal",
			UUID:      "royal-node-uuid",
			Name:      "royal-legacy",
			Address:   netip.MustParseAddr("37.16.74.103"),
			DNSZones:  []string{"royal.example.com"},
		},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	if len(mode.keyboard.Inline) != 2 || !strings.Contains(mode.keyboard.Inline[1][0].Text, "Royal") {
		t.Fatalf("IP mode keyboard = %#v", mode.keyboard)
	}
	handleCallback(t, controller, "royal-mode", mode.keyboard.Inline[1][0].CallbackData, mode.message)
	edits := messenger.editsSnapshot()
	scope := edits[len(edits)-1]
	handleCallback(t, controller, "with-node", scope.keyboard.Inline[0][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "royal-legacy")
	if got := messenger.lastSent().text; !strings.Contains(got, "шлюз x.x.x.1") || !strings.Contains(got, "обновит ноду и DNS") {
		t.Fatalf("Royal new-IP prompt = %q", got)
	}
	handleMessage(t, controller, 3, "47.23.12.146")
	password := &Message{ID: 4, ChatID: testChatID, FromUserID: testAllowedUser, Text: "root-secret"}
	if err := controller.Handle(context.Background(), Update{Message: password}); err != nil {
		t.Fatal(err)
	}
	controller.Wait()
	if application.royalCalls != 1 || application.royalInput.ServerIP.String() != "37.16.74.103" || application.royalInput.NewIP.String() != "47.23.12.146" || string(application.royalPassword) != "root-secret" {
		t.Fatalf("Royal input=%#v calls=%d", application.royalInput, application.royalCalls)
	}
	if application.replaceCalls != 1 || application.replaceInput.NodeUUID != "royal-node-uuid" || application.replaceInput.NewIP.String() != "47.23.12.146" {
		t.Fatalf("replace input=%#v calls=%d", application.replaceInput, application.replaceCalls)
	}
	edits = messenger.editsSnapshot()
	result := edits[len(edits)-1].text
	if !strings.Contains(result, "47.23.12.1") || !strings.Contains(result, "SSH по новому IP подтверждён") {
		t.Fatalf("Royal result = %q", result)
	}
}

func TestRoyalIPWizardWorksBeforeNodeIsAddedToRemnawave(t *testing.T) {
	application := &fakeRoyalIPApplication{fakeApplication: &fakeApplication{}}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	mode := messenger.lastSent()
	handleCallback(t, controller, "royal-mode", mode.keyboard.Inline[0][0].CallbackData, mode.message)
	edits := messenger.editsSnapshot()
	scope := edits[len(edits)-1]
	handleCallback(t, controller, "without-node", scope.keyboard.Inline[1][0].CallbackData, mode.message)
	handleMessage(t, controller, 2, "37.16.74.103")
	handleMessage(t, controller, 3, "47.23.12.146")
	password := &Message{ID: 4, ChatID: testChatID, FromUserID: testAllowedUser, Text: "root-secret"}
	if err := controller.Handle(context.Background(), Update{Message: password}); err != nil {
		t.Fatal(err)
	}
	controller.Wait()
	if application.calls != 1 || application.input.ServerIP.String() != "37.16.74.103" || application.input.NewIP.String() != "47.23.12.146" || string(application.password) != "root-secret" {
		t.Fatalf("Royal input=%#v calls=%d", application.input, application.calls)
	}
}

func TestDeploymentCardRepairsCertificateWithoutManualUUIDOrSNI(t *testing.T) {
	const deploymentID = "1c60754a-f9bb-41fa-95bb-39f11375bbaa"
	base := &fakeApplication{deployments: []DeploymentSummary{{ID: deploymentID, PanelName: "Hit", NodeName: "de-new-0", Status: "FAILED"}}}
	application := &fakeRecoveryApplication{fakeApplication: base, details: DeploymentDetails{DeploymentSummary: DeploymentSummary{ID: deploymentID, PanelName: "Hit", NodeName: "de-new-0", Status: "FAILED", CurrentStep: "prepare_certificate"}, SNI: "nl-modx-roy.nodexphere.net", CanRetryStep: true, CanRepairCert: true}, bootstrapResult: "Certificate ready"}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuDeployments)
	list := messenger.lastSent()
	handleCallback(t, controller, "open", list.keyboard.Inline[0][0].CallbackData, list.message)
	edits := messenger.editsSnapshot()
	card := edits[len(edits)-1]
	if strings.Contains(card.text, deploymentID) || strings.Contains(card.text, "nl-modx-roy.nodexphere.net") {
		t.Fatalf("card exposed values the operator should not copy manually: %q", card.text)
	}
	handleCallback(t, controller, "repair", card.keyboard.Inline[1][0].CallbackData, list.message)
	controller.Wait()
	if application.bootstrapSNI != "nl-modx-roy.nodexphere.net" || application.retryCalls != 1 {
		t.Fatalf("repair used SNI=%q retries=%d", application.bootstrapSNI, application.retryCalls)
	}
}

func TestDeploymentLogsButtonShowsDetailedJournalAndBackNavigation(t *testing.T) {
	const deploymentID = "1c60754a-f9bb-41fa-95bb-39f11375bbaa"
	started := time.Date(2026, 8, 17, 18, 20, 0, 0, time.UTC)
	completed := started.Add(5 * time.Second)
	summary := DeploymentSummary{ID: deploymentID, PanelName: "Hit", NodeName: "node", Status: "FAILED", CurrentStep: "provisioning", SafeErrorCode: "PROVISIONING_FAILED", SafeError: "VPS provisioning failed", UpdatedAt: completed}
	base := &fakeApplication{deployments: []DeploymentSummary{summary}}
	application := &fakeRecoveryApplication{
		fakeApplication: base,
		details:         DeploymentDetails{DeploymentSummary: summary, CanRetryStep: true},
		logs:            []DeploymentLogEntry{{Step: "logrotate", Status: "FAILED", Summary: "validation failed", Code: "E-PROVISIONING-LOGROTATE", StartedAt: &started, CompletedAt: &completed}},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuDeployments)
	list := messenger.lastSent()
	handleCallback(t, controller, "open", list.keyboard.Inline[0][0].CallbackData, list.message)
	edits := messenger.editsSnapshot()
	card := edits[len(edits)-1]
	handleCallback(t, controller, "logs", card.keyboard.Inline[0][0].CallbackData, list.message)
	edits = messenger.editsSnapshot()
	logs := edits[len(edits)-1]
	if !strings.Contains(logs.text, "Подробный журнал") || !strings.Contains(logs.text, "E-PROVISIONING-LOGROTATE") {
		t.Fatalf("logs = %q", logs.text)
	}
	if len(logs.keyboard.Inline) != 2 || logs.keyboard.Inline[0][0].Text != "🔄 Обновить журнал" || logs.keyboard.Inline[1][0].Text != "⬅️ К карточке" {
		t.Fatalf("logs keyboard = %#v", logs.keyboard)
	}
}

func TestAddNodeWizardTransitionsAndDeploymentProgress(t *testing.T) {
	profileReady := ReadinessReady
	application := &fakeApplication{
		hosts: []Host{{ID: "host-internal-id", Remark: "DE Frankfurt", Address: "edge.example.com", ConfigProfileReadiness: profileReady}},
		preflightResult: PreflightResult{
			PreparedDeploymentID:   "prepared-internal-id",
			DNSZone:                "edge.example.com",
			CertificateReadiness:   ReadinessReady,
			ConfigProfileReadiness: ReadinessReady,
			Warnings:               []OperatorNotice{{Code: "W-DOCKER-NOT-INSTALLED", Message: "Docker будет установлен автоматически"}},
		},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	ctx := context.Background()

	handleMessage(t, controller, 1, MenuAddNode)
	hostPicker := messenger.lastSent()
	if len(hostPicker.keyboard.Inline) != 1 || len(hostPicker.keyboard.Inline[0]) != 1 {
		t.Fatalf("Host keyboard = %#v", hostPicker.keyboard)
	}
	hostCallback := hostPicker.keyboard.Inline[0][0].CallbackData
	if strings.Contains(hostCallback, "host-internal-id") {
		t.Fatalf("callback_data exposed Host identifier: %q", hostCallback)
	}
	handleCallback(t, controller, "host", hostCallback, hostPicker.message)

	handleMessage(t, controller, 2, "x")
	if application.nameChecks != 0 || !strings.Contains(messenger.lastSent().text, "Invalid Node name") {
		t.Fatalf("invalid name was not rejected locally")
	}
	application.nameErr = ErrDuplicateNodeName
	handleMessage(t, controller, 3, "node-frankfurt")
	if !strings.Contains(messenger.lastSent().text, "already exists") {
		t.Fatalf("duplicate name response = %q", messenger.lastSent().text)
	}
	application.nameErr = nil
	handleMessage(t, controller, 4, "node-frankfurt")
	if !strings.Contains(messenger.lastSent().text, "public VPS IP") {
		t.Fatalf("name transition response = %q", messenger.lastSent().text)
	}

	handleMessage(t, controller, 5, "10.0.0.1")
	if application.addressChecks != 0 || !strings.Contains(messenger.lastSent().text, "publicly routable") {
		t.Fatalf("private address was not rejected locally")
	}
	application.addressErr = ErrDuplicateVPSIP
	handleMessage(t, controller, 6, "8.8.8.8")
	if !strings.Contains(messenger.lastSent().text, "already exists") {
		t.Fatalf("duplicate address response = %q", messenger.lastSent().text)
	}
	application.addressErr = nil
	handleMessage(t, controller, 7, "8.8.8.8")
	if !strings.Contains(messenger.lastSent().text, "temporary root password") {
		t.Fatalf("IP transition response = %q", messenger.lastSent().text)
	}

	passwordMessage := &Message{ID: 8, ChatID: testChatID, FromUserID: testAllowedUser, Text: "temporary-super-secret"}
	if err := controller.Handle(ctx, Update{Message: passwordMessage}); err != nil {
		t.Fatalf("Handle(password) error = %v", err)
	}
	if passwordMessage.Text != "" {
		t.Fatalf("password update text was retained")
	}
	if !messenger.wasDeleted(testChatID, 8) {
		t.Fatalf("password message was not deleted")
	}
	if got := string(application.preflightPassword); got != "temporary-super-secret" {
		t.Fatalf("preflight password = %q", got)
	}
	confirmation := messenger.lastSent()
	for _, expected := range []string{"DE Frankfurt", "SNI: edge.example.com", "node-frankfurt", "8.8.8.8", "DNS zone: edge.example.com", "Certificate: ready", "Config profile: ready"} {
		if !strings.Contains(confirmation.text, expected) {
			t.Fatalf("confirmation missing %q:\n%s", expected, confirmation.text)
		}
	}
	if strings.Contains(confirmation.text, "temporary-super-secret") || strings.Contains(confirmation.text, "prepared-internal-id") || strings.Contains(confirmation.text, "host-internal-id") {
		t.Fatalf("confirmation exposed internal or secret data: %q", confirmation.text)
	}
	if len(confirmation.keyboard.Inline) != 2 {
		t.Fatalf("confirmation keyboard = %#v", confirmation.keyboard)
	}
	for _, row := range confirmation.keyboard.Inline {
		for _, button := range row {
			for _, forbidden := range []string{"temporary-super-secret", "prepared-internal-id", "host-internal-id", "8.8.8.8"} {
				if strings.Contains(button.CallbackData, forbidden) {
					t.Fatalf("callback_data %q contains %q", button.CallbackData, forbidden)
				}
			}
		}
	}

	deployCallback := confirmation.keyboard.Inline[0][0].CallbackData
	handleCallback(t, controller, "deploy", deployCallback, confirmation.message)
	controller.Wait()
	if application.deployCalls != 1 || string(application.deploymentPassword) != "temporary-super-secret" {
		t.Fatalf("deployment calls=%d password=%q", application.deployCalls, application.deploymentPassword)
	}
	if application.deploymentInput.PreparedDeploymentID != "prepared-internal-id" || application.deploymentInput.VPSIP != netip.MustParseAddr("8.8.8.8") {
		t.Fatalf("deployment input = %#v", application.deploymentInput)
	}
	edits := messenger.editsSnapshot()
	if len(edits) < 4 {
		t.Fatalf("status edits = %d, want starting, progress and completion", len(edits))
	}
	for _, edit := range edits {
		if edit.chatID != testChatID || edit.messageID != confirmation.message.ID {
			t.Fatalf("progress used another message: %#v", edit)
		}
	}
	if got := edits[len(edits)-1].text; !strings.Contains(got, "✅ Нода успешно развёрнута") || !strings.Contains(got, "Прогресс: 6/6") || !strings.Contains(got, "W-DOCKER-NOT-INSTALLED") {
		t.Fatalf("final status = %q", got)
	}
}

func TestExpiredWizardClearsPasswordAndRejectsIncompleteConversation(t *testing.T) {
	now := time.Unix(100, 0)
	application := &fakeApplication{hosts: []Host{{ID: "host", Remark: "Host", Address: "host.example.com"}}}
	messenger := &fakeMessenger{}
	controller, err := newController([]int64{testAllowedUser}, application, messenger, time.Minute, func() time.Time { return now }, func() (string, error) { return "fixed-nonce", nil })
	if err != nil {
		t.Fatal(err)
	}
	handleMessage(t, controller, 1, MenuAddNode)

	controller.mu.Lock()
	session := controller.sessions[testAllowedUser]
	session.state = stateAwaitingConfirmation
	session.password = []byte("must-be-cleared")
	session.preflight.PreparedDeploymentID = "prepared-expired"
	passwordBacking := session.password
	controller.mu.Unlock()

	now = now.Add(2 * time.Minute)
	handleMessage(t, controller, 2, "late message")
	if !strings.Contains(messenger.lastSent().text, "expired") {
		t.Fatalf("expiry response = %q", messenger.lastSent().text)
	}
	for index, value := range passwordBacking {
		if value != 0 {
			t.Fatalf("expired password byte %d was not cleared", index)
		}
	}
	if application.preflightCalls != 0 {
		t.Fatalf("expired wizard triggered preflight")
	}
	if application.cancelCalls != 1 {
		t.Fatalf("expired prepared deployment cancel calls = %d", application.cancelCalls)
	}

	stale := &CallbackQuery{ID: "stale", FromUserID: testAllowedUser, Message: &Message{ID: 50, ChatID: testChatID}, Data: "add:deploy:fixed-nonce"}
	if err := controller.Handle(context.Background(), Update{CallbackQuery: stale}); err != nil {
		t.Fatalf("stale callback error = %v", err)
	}
	if !strings.Contains(messenger.lastSent().text, "incomplete or expired") {
		t.Fatalf("stale callback response = %q", messenger.lastSent().text)
	}
}

func TestPublicIPValidation(t *testing.T) {
	for _, invalid := range []string{"", "not-an-ip", "127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.10", "2001:db8::1", "fe80::1"} {
		if _, valid := parsePublicIP(invalid); valid {
			t.Errorf("parsePublicIP(%q) unexpectedly valid", invalid)
		}
	}
	for _, valid := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if _, ok := parsePublicIP(valid); !ok {
			t.Errorf("parsePublicIP(%q) unexpectedly invalid", valid)
		}
	}
}

func testController(t *testing.T, app Application, messenger *fakeMessenger, now func() time.Time) *Controller {
	t.Helper()
	controller, err := newController([]int64{testAllowedUser}, app, messenger, 15*time.Minute, now, func() (string, error) { return "fixed-nonce", nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	return controller
}

func handleMessage(t *testing.T, controller *Controller, messageID int, text string) {
	t.Helper()
	if err := controller.Handle(context.Background(), Update{Message: &Message{ID: messageID, ChatID: testChatID, FromUserID: testAllowedUser, Text: text}}); err != nil {
		t.Fatalf("Handle(%q) error = %v", text, err)
	}
}

func handleCallback(t *testing.T, controller *Controller, id, data string, message Message) {
	t.Helper()
	message.FromUserID = testAllowedUser
	callback := &CallbackQuery{ID: id, FromUserID: testAllowedUser, Message: &message, Data: data}
	if err := controller.Handle(context.Background(), Update{CallbackQuery: callback}); err != nil {
		t.Fatalf("Handle(callback %q) error = %v", data, err)
	}
}

type fakeApplication struct {
	mu sync.Mutex

	hosts           []Host
	panels          []Panel
	nodes           []NodeSummary
	nameErr         error
	addressErr      error
	preflightResult PreflightResult
	preflightErr    error
	deployErr       error

	listHostCalls       int
	nameChecks          int
	addressChecks       int
	preflightCalls      int
	deployCalls         int
	cancelCalls         int
	listNodeCalls       int
	listDeploymentCalls int
	preflightPassword   []byte
	deploymentPassword  []byte
	deploymentInput     DeploymentInput
	deployments         []DeploymentSummary
}

type fakeRecoveryApplication struct {
	*fakeApplication
	bootstrapCalls    int
	bootstrapSNI      string
	bootstrapOperator int64
	bootstrapResult   string
	bootstrapErr      error
	details           DeploymentDetails
	retryCalls        int
	logs              []DeploymentLogEntry
}

func (f *fakeRecoveryApplication) RetryFailedStep(context.Context, string) error {
	f.retryCalls++
	return nil
}
func (f *fakeRecoveryApplication) RetryDNS(context.Context, string) error { return nil }
func (f *fakeRecoveryApplication) GetDeploymentDetails(context.Context, string) (DeploymentDetails, error) {
	return f.details, nil
}

func (f *fakeRecoveryApplication) RecheckRemnawave(context.Context, string) (string, error) {
	return "checked", nil
}

type fakeNodeIPApplication struct {
	*fakeApplication
	target       NodeIPChangeTarget
	findErr      error
	replaceErr   error
	replaceCalls int
	input        NodeIPChangeInput
	findPanel    string
	findQuery    string
}

type fakeDNSSyncApplication struct {
	*fakeApplication
	target    NodeDNSSyncTarget
	findErr   error
	result    NodeDNSSyncResult
	syncErr   error
	syncCalls int
	input     NodeDNSSyncInput
}

type fakeCherryIPApplication struct {
	*fakeApplication
	calls    int
	input    CherryIPInput
	password []byte
}

type fakeCherryNodeApplication struct {
	*fakeApplication
	target         NodeIPChangeTarget
	findErr        error
	replaceErr     error
	findPanel      string
	replaceCalls   int
	replaceInput   NodeIPChangeInput
	cherryCalls    int
	cherryInput    CherryIPInput
	cherryPassword []byte
}

type fakeRoyalNodeApplication struct {
	*fakeApplication
	target        NodeIPChangeTarget
	replaceCalls  int
	replaceInput  NodeIPChangeInput
	royalCalls    int
	royalInput    RoyalIPInput
	royalPassword []byte
}

type fakeRoyalIPApplication struct {
	*fakeApplication
	calls    int
	input    RoyalIPInput
	password []byte
}

func (f *fakeCherryIPApplication) ConfigureCherryIP(_ context.Context, input CherryIPInput) (CherryIPResult, error) {
	f.calls++
	f.input = input
	f.input.Password = nil
	f.password = append([]byte(nil), input.Password...)
	return CherryIPResult{Interface: "ens3", LiveConfigured: true, Persistent: true}, nil
}

func (f *fakeCherryNodeApplication) FindNodeForIPChange(_ context.Context, panelID, _ string) (NodeIPChangeTarget, error) {
	f.findPanel = panelID
	return f.target, f.findErr
}

func (f *fakeCherryNodeApplication) ReplaceNodeIP(_ context.Context, input NodeIPChangeInput) (string, error) {
	f.replaceCalls++
	f.replaceInput = input
	return "Нода и DNS обновлены", f.replaceErr
}

func (f *fakeCherryNodeApplication) ConfigureCherryIP(_ context.Context, input CherryIPInput) (CherryIPResult, error) {
	f.cherryCalls++
	f.cherryInput = input
	f.cherryInput.Password = nil
	f.cherryPassword = append([]byte(nil), input.Password...)
	return CherryIPResult{Interface: "ens3", LiveConfigured: true, Persistent: true}, nil
}

func (f *fakeRoyalNodeApplication) FindNodeForIPChange(context.Context, string, string) (NodeIPChangeTarget, error) {
	return f.target, nil
}

func (f *fakeRoyalNodeApplication) ReplaceNodeIP(_ context.Context, input NodeIPChangeInput) (string, error) {
	f.replaceCalls++
	f.replaceInput = input
	return "Нода и DNS обновлены", nil
}

func (f *fakeRoyalNodeApplication) ConfigureRoyalIP(_ context.Context, input RoyalIPInput) (RoyalIPResult, error) {
	f.royalCalls++
	f.royalInput = input
	f.royalInput.Password = nil
	f.royalPassword = append([]byte(nil), input.Password...)
	return RoyalIPResult{Interface: "eth0", PrefixBits: 24, Gateway: netip.MustParseAddr("47.23.12.1"), NetplanFile: "/etc/netplan/50-cloud-init.yaml", BackupFile: "/etc/netplan/50-cloud-init.yaml.bak-royalbot"}, nil
}

func (f *fakeRoyalIPApplication) ConfigureRoyalIP(_ context.Context, input RoyalIPInput) (RoyalIPResult, error) {
	f.calls++
	f.input = input
	f.input.Password = nil
	f.password = append([]byte(nil), input.Password...)
	return RoyalIPResult{Interface: "eth0", PrefixBits: 24, Gateway: netip.MustParseAddr("47.23.12.1"), NetplanFile: "/etc/netplan/50-cloud-init.yaml", BackupFile: "/etc/netplan/50-cloud-init.yaml.bak-royalbot"}, nil
}

func (f *fakeNodeIPApplication) FindNodeForIPChange(_ context.Context, panelID, query string) (NodeIPChangeTarget, error) {
	f.findPanel = panelID
	f.findQuery = query
	return f.target, f.findErr
}
func (f *fakeNodeIPApplication) ReplaceNodeIP(_ context.Context, input NodeIPChangeInput) (string, error) {
	f.replaceCalls++
	f.input = input
	return "IP изменён", f.replaceErr
}

func (f *fakeDNSSyncApplication) FindNodeForDNSSync(context.Context, string, string) (NodeDNSSyncTarget, error) {
	return f.target, f.findErr
}

func (f *fakeDNSSyncApplication) SyncNodeDNS(_ context.Context, input NodeDNSSyncInput) (NodeDNSSyncResult, error) {
	f.syncCalls++
	f.input = input
	return f.result, f.syncErr
}
func (f *fakeRecoveryApplication) ViewSafeLogs(context.Context, string) ([]DeploymentLogEntry, error) {
	return append([]DeploymentLogEntry(nil), f.logs...), nil
}
func (f *fakeRecoveryApplication) BootstrapCertificate(_ context.Context, sni string, operatorUserID int64) (string, error) {
	f.bootstrapCalls++
	f.bootstrapSNI = sni
	f.bootstrapOperator = operatorUserID
	return f.bootstrapResult, f.bootstrapErr
}

func (f *fakeApplication) ListPanels(context.Context) ([]Panel, error) {
	if len(f.panels) != 0 {
		return append([]Panel(nil), f.panels...), nil
	}
	return []Panel{{ID: "default", Name: "Default", DNSEnabled: true}}, nil
}

func (f *fakeApplication) ListHosts(context.Context, string) ([]Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listHostCalls++
	return append([]Host(nil), f.hosts...), nil
}

func (f *fakeApplication) CheckNodeName(_ context.Context, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nameChecks++
	return f.nameErr
}

func (f *fakeApplication) CheckVPSAddress(_ context.Context, _ string, _ netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addressChecks++
	return f.addressErr
}

func (f *fakeApplication) Preflight(_ context.Context, input PreflightInput) (PreflightResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.preflightCalls++
	f.preflightPassword = append([]byte(nil), input.Password...)
	return f.preflightResult, f.preflightErr
}

func (f *fakeApplication) StartDeployment(_ context.Context, input DeploymentInput, progress func(Progress) error) error {
	f.mu.Lock()
	f.deployCalls++
	f.deploymentPassword = append([]byte(nil), input.Password...)
	f.deploymentInput = input
	f.deploymentInput.Password = nil
	f.mu.Unlock()
	if err := progress(Progress{Step: "preflight", Completed: 1, Total: 2, SafeMessage: "Preflight complete", Status: ProgressCompleted}); err != nil {
		return err
	}
	if err := progress(Progress{Step: "provisioning", Completed: 2, Total: 2, SafeMessage: "Provisioning complete", Status: ProgressCompleted}); err != nil {
		return err
	}
	return f.deployErr
}

func (f *fakeApplication) CancelDeployment(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelCalls++
	return nil
}

func (f *fakeApplication) ListNodes(context.Context) ([]NodeSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listNodeCalls++
	return append([]NodeSummary(nil), f.nodes...), nil
}

func (f *fakeApplication) ListDeployments(context.Context, int) ([]DeploymentSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listDeploymentCalls++
	return append([]DeploymentSummary(nil), f.deployments...), nil
}

func (f *fakeApplication) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listHostCalls + f.nameChecks + f.addressChecks + f.preflightCalls + f.deployCalls + f.cancelCalls + f.listNodeCalls + f.listDeploymentCalls
}

type sentRecord struct {
	message  Message
	text     string
	keyboard Keyboard
}

type editRecord struct {
	chatID    int64
	messageID int
	text      string
	keyboard  Keyboard
}

type callbackAnswer struct {
	id   string
	text string
}

type fakeMessenger struct {
	mu sync.Mutex

	nextID    int
	sent      []sentRecord
	edits     []editRecord
	deleted   [][2]int64
	answers   []callbackAnswer
	deleteErr error
	editErr   error
}

func (f *fakeMessenger) SendMessage(_ context.Context, chatID int64, text string, keyboard Keyboard) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	message := Message{ID: f.nextID, ChatID: chatID}
	f.sent = append(f.sent, sentRecord{message: message, text: text, keyboard: keyboard})
	return message, nil
}

func (f *fakeMessenger) EditMessage(_ context.Context, chatID int64, messageID int, text string, keyboard Keyboard) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edits = append(f.edits, editRecord{chatID: chatID, messageID: messageID, text: text, keyboard: keyboard})
	return f.editErr
}

func (f *fakeMessenger) DeleteMessage(_ context.Context, chatID int64, messageID int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, [2]int64{chatID, int64(messageID)})
	return f.deleteErr
}

func (f *fakeMessenger) AnswerCallback(_ context.Context, callbackID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, callbackAnswer{id: callbackID, text: text})
	return nil
}

func (f *fakeMessenger) lastSent() sentRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentRecord{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeMessenger) lastAnswer() callbackAnswer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return callbackAnswer{}
	}
	return f.answers[len(f.answers)-1]
}

func (f *fakeMessenger) wasDeleted(chatID int64, messageID int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, item := range f.deleted {
		if item == [2]int64{chatID, int64(messageID)} {
			return true
		}
	}
	return false
}

func (f *fakeMessenger) editsSnapshot() []editRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]editRecord(nil), f.edits...)
}

var _ Application = (*fakeApplication)(nil)
var _ Messenger = (*fakeMessenger)(nil)
