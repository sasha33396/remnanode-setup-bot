package telegram

import (
	"context"
	"errors"
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
		target: NodeIPChangeTarget{PanelName: "Test", UUID: "node-uuid", Name: "legacy", Address: netip.MustParseAddr("8.8.8.8")},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuChangeIP)
	picker := messenger.lastSent()
	handleCallback(t, controller, "panel", picker.keyboard.Inline[1][0].CallbackData, picker.message)
	handleMessage(t, controller, 2, "legacy")
	if application.findPanel != "test" { t.Fatalf("find panel = %q", application.findPanel) }
	card := messenger.lastSent()
	if !strings.Contains(card.text, "Панель: Test") { t.Fatalf("card = %q", card.text) }
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
			SafeWarnings:           []string{"Docker will be installed"},
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
	if got := edits[len(edits)-1].text; got != "✅ Deployment completed." {
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
}

type fakeRecoveryApplication struct {
	*fakeApplication
	bootstrapCalls    int
	bootstrapSNI      string
	bootstrapOperator int64
	bootstrapResult   string
	bootstrapErr      error
}

func (f *fakeRecoveryApplication) RetryFailedStep(context.Context, string) error { return nil }
func (f *fakeRecoveryApplication) RetryDNS(context.Context, string) error        { return nil }

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
}

func (f *fakeNodeIPApplication) FindNodeForIPChange(_ context.Context, panelID, _ string) (NodeIPChangeTarget, error) {
	f.findPanel = panelID
	return f.target, f.findErr
}
func (f *fakeNodeIPApplication) ReplaceNodeIP(_ context.Context, input NodeIPChangeInput) (string, error) {
	f.replaceCalls++
	f.input = input
	return "IP изменён", f.replaceErr
}
func (f *fakeRecoveryApplication) ViewSafeLogs(context.Context, string) ([]string, error) {
	return nil, nil
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
	if err := progress(Progress{Step: "preflight", Completed: 1, Total: 2, SafeMessage: "Preflight complete"}); err != nil {
		return err
	}
	if err := progress(Progress{Step: "provisioning", Completed: 2, Total: 2, SafeMessage: "Provisioning complete"}); err != nil {
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
	return nil, nil
}

func (f *fakeApplication) ListDeployments(context.Context, int) ([]DeploymentSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listDeploymentCalls++
	return nil, nil
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
	return nil
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
