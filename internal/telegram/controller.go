package telegram

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	defaultSessionTTL = 15 * time.Minute
	cancelTimeout     = 10 * time.Second
	maxPasswordBytes  = 4096
	maxMessageBytes   = 3900
)

// Controller owns Telegram authorization, presentation and transient wizard
// state. It contains no infrastructure implementations.
type Controller struct {
	app       Application
	messenger Messenger
	allowed   map[int64]struct{}
	ttl       time.Duration
	now       func() time.Time
	nonce     func() (string, error)

	mu       sync.Mutex
	sessions map[int64]*wizard
	workers  sync.WaitGroup
}

var _ UpdateHandler = (*Controller)(nil)

func NewController(allowedUsers []int64, app Application, messenger Messenger, sessionTTL time.Duration) (*Controller, error) {
	return newController(allowedUsers, app, messenger, sessionTTL, time.Now, randomNonce)
}

func newController(allowedUsers []int64, app Application, messenger Messenger, sessionTTL time.Duration, now func() time.Time, nonce func() (string, error)) (*Controller, error) {
	if app == nil || messenger == nil || now == nil || nonce == nil || len(allowedUsers) == 0 {
		return nil, errors.New("invalid Telegram controller configuration")
	}
	if sessionTTL <= 0 {
		sessionTTL = defaultSessionTTL
	}
	allowed := make(map[int64]struct{}, len(allowedUsers))
	for _, userID := range allowedUsers {
		if userID <= 0 {
			return nil, errors.New("invalid allowed Telegram user ID")
		}
		allowed[userID] = struct{}{}
	}
	return &Controller{
		app:       app,
		messenger: messenger,
		allowed:   allowed,
		ttl:       sessionTTL,
		now:       now,
		nonce:     nonce,
		sessions:  make(map[int64]*wizard),
	}, nil
}

// Handle processes one update. Callers may invoke Handle concurrently.
func (c *Controller) Handle(ctx context.Context, update Update) error {
	if update.CallbackQuery != nil {
		return c.handleCallback(ctx, update.CallbackQuery)
	}
	if update.Message != nil {
		return c.handleMessage(ctx, update.Message)
	}
	return nil
}

// Wait blocks until deployments started by callbacks have finished. Call it
// only after no further updates can start new deployments.
func (c *Controller) Wait() { c.workers.Wait() }

// Close destroys all transient passwords. It does not cancel deployments that
// have already been handed to the application.
func (c *Controller) Close() {
	c.mu.Lock()
	prepared := make([]string, 0, len(c.sessions))
	for userID, session := range c.sessions {
		prepared = append(prepared, session.preflight.PreparedDeploymentID)
		session.clear()
		delete(c.sessions, userID)
	}
	c.mu.Unlock()
	for _, deploymentID := range prepared {
		c.cancelPrepared(deploymentID)
	}
}

func (c *Controller) handleMessage(ctx context.Context, message *Message) error {
	if message.FromUserID <= 0 || message.ChatID == 0 {
		return nil
	}
	if !c.authorized(message.FromUserID) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Access denied.", Keyboard{})
		return err
	}

	text := strings.TrimSpace(message.Text)
	if handled, err := c.handleRecoveryCommand(ctx, message, text); handled {
		return err
	}
	switch text {
	case "/start", "/menu":
		c.cancelExisting(ctx, message.FromUserID)
		return c.showMainMenu(ctx, message.ChatID)
	case "/cancel":
		return c.cancelFromMessage(ctx, message)
	case MenuAddNode:
		return c.beginAddNode(ctx, message)
	case MenuNodes:
		c.cancelExisting(ctx, message.FromUserID)
		return c.showNodes(ctx, message.ChatID)
	case MenuDeployments:
		c.cancelExisting(ctx, message.FromUserID)
		return c.showDeployments(ctx, message.ChatID)
	}

	session, found, expired := c.loadSession(message.FromUserID)
	if expired {
		return c.sendExpired(ctx, message.ChatID)
	}
	if !found || session.chatID != message.ChatID {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "No active conversation. Choose an action from the menu.", mainKeyboard())
		return err
	}

	switch session.state {
	case stateSelectingHost:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Select a Host using the inline buttons.", Keyboard{})
		return err
	case stateAwaitingName:
		return c.acceptNodeName(ctx, message, session)
	case stateAwaitingIP:
		return c.acceptVPSIP(ctx, message, session)
	case stateAwaitingPassword:
		return c.acceptPassword(ctx, message, session)
	case statePreflighting:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Preflight is already running.", Keyboard{})
		return err
	case stateAwaitingConfirmation:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Use Deploy or Cancel on the confirmation message.", Keyboard{})
		return err
	default:
		return c.sendExpired(ctx, message.ChatID)
	}
}

func (c *Controller) handleRecoveryCommand(ctx context.Context, message *Message, text string) (bool, error) {
	commands := []string{"/retry_step", "/retry_dns", "/recheck", "/logs", "/cancel_deployment"}
	var command, deploymentID string
	for _, candidate := range commands {
		if text == candidate || strings.HasPrefix(text, candidate+" ") {
			command = candidate
			deploymentID = strings.TrimSpace(strings.TrimPrefix(text, candidate))
			break
		}
	}
	if command == "" {
		return false, nil
	}
	if !validDeploymentID(deploymentID) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Provide a valid deployment UUID.", mainKeyboard())
		return true, err
	}
	if command == "/cancel_deployment" {
		err := c.app.CancelDeployment(ctx, deploymentID)
		return true, c.sendActionResult(ctx, message.ChatID, err, "Deployment cancelled.")
	}
	recoveryApp, ok := c.app.(RecoveryApplication)
	if !ok {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Recovery actions are unavailable.", mainKeyboard())
		return true, err
	}
	switch command {
	case "/retry_step":
		return true, c.sendActionResult(ctx, message.ChatID, recoveryApp.RetryFailedStep(ctx, deploymentID), "Failed step retry completed.")
	case "/retry_dns":
		return true, c.sendActionResult(ctx, message.ChatID, recoveryApp.RetryDNS(ctx, deploymentID), "DNS retry completed.")
	case "/recheck":
		result, err := recoveryApp.RecheckRemnawave(ctx, deploymentID)
		if err != nil {
			return true, c.sendActionResult(ctx, message.ChatID, err, "")
		}
		_, err = c.messenger.SendMessage(ctx, message.ChatID, safeLine(result, 300), mainKeyboard())
		return true, err
	case "/logs":
		lines, err := recoveryApp.ViewSafeLogs(ctx, deploymentID)
		if err != nil {
			return true, c.sendActionResult(ctx, message.ChatID, err, "")
		}
		if len(lines) == 0 {
			lines = []string{"No safe log entries."}
		}
		_, err = c.messenger.SendMessage(ctx, message.ChatID, truncateUTF8(strings.Join(lines, "\n"), maxMessageBytes), mainKeyboard())
		return true, err
	default:
		return false, nil
	}
}

func (c *Controller) sendActionResult(ctx context.Context, chatID int64, actionErr error, success string) error {
	message := success
	if actionErr != nil {
		message = "Operation could not be completed safely. Check deployment logs."
	}
	_, err := c.messenger.SendMessage(ctx, chatID, message, mainKeyboard())
	return err
}

func validDeploymentID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') && !(character >= 'A' && character <= 'F') {
			return false
		}
	}
	return true
}

func (c *Controller) handleCallback(ctx context.Context, callback *CallbackQuery) error {
	if callback.FromUserID <= 0 {
		return nil
	}
	if !c.authorized(callback.FromUserID) {
		return c.messenger.AnswerCallback(ctx, callback.ID, "Access denied.")
	}
	if callback.Message == nil || callback.Message.ChatID == 0 {
		return c.messenger.AnswerCallback(ctx, callback.ID, "This action is no longer available.")
	}
	action, nonce, index, valid := parseCallbackData(callback.Data)
	if !valid {
		return c.expiredCallback(ctx, callback)
	}
	session, found, expired := c.loadSession(callback.FromUserID)
	if expired || !found || session.nonce != nonce || session.chatID != callback.Message.ChatID {
		return c.expiredCallback(ctx, callback)
	}
	_ = c.messenger.AnswerCallback(ctx, callback.ID, "")

	switch action {
	case "host":
		if session.state != stateSelectingHost || index < 0 || index >= len(session.hosts) {
			return c.expiredCallback(ctx, callback)
		}
		selected := session.hosts[index]
		if !c.updateSession(callback.FromUserID, nonce, stateSelectingHost, func(current *wizard) {
			current.selected = selected
			current.state = stateAwaitingName
		}) {
			return c.expiredCallback(ctx, callback)
		}
		_, err := c.messenger.SendMessage(ctx, session.chatID, "Enter the Node name manually (3–30 characters).", Keyboard{})
		return err
	case "deploy":
		return c.startDeployment(ctx, callback, session)
	case "cancel":
		return c.cancelFromCallback(ctx, callback, session)
	default:
		return c.expiredCallback(ctx, callback)
	}
}

func (c *Controller) beginAddNode(ctx context.Context, message *Message) error {
	c.cancelExisting(ctx, message.FromUserID)
	hosts, err := c.app.ListHosts(ctx)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Hosts are temporarily unavailable. Try again later.", mainKeyboard())
		return sendErr
	}
	selectable := make([]Host, 0, len(hosts))
	for _, host := range hosts {
		if strings.TrimSpace(host.ID) == "" || strings.TrimSpace(host.Remark) == "" || strings.TrimSpace(host.Address) == "" {
			continue
		}
		selectable = append(selectable, host)
	}
	if len(selectable) == 0 {
		_, err = c.messenger.SendMessage(ctx, message.ChatID, "No deployable Hosts are available.", mainKeyboard())
		return err
	}
	nonce, err := c.nonce()
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Could not start the wizard. Try again.", mainKeyboard())
		return errors.Join(errors.New("generate Telegram wizard nonce"), sendErr)
	}
	session := &wizard{
		userID:    message.FromUserID,
		chatID:    message.ChatID,
		nonce:     nonce,
		state:     stateSelectingHost,
		expiresAt: c.now().Add(c.ttl),
		hosts:     selectable,
	}
	c.putSession(session)

	rows := make([][]Button, 0, len(selectable))
	for index, host := range selectable {
		rows = append(rows, []Button{{
			Text:         safeLine(host.Remark, 48),
			CallbackData: fmt.Sprintf("add:host:%s:%d", nonce, index),
		}})
	}
	_, err = c.messenger.SendMessage(ctx, message.ChatID, "Select a Remnawave Host:", Keyboard{Inline: rows})
	return err
}

func (c *Controller) acceptNodeName(ctx context.Context, message *Message, session *wizard) error {
	name := strings.TrimSpace(message.Text)
	if !validNodeName(name) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Invalid Node name. Use 3–30 printable characters.", Keyboard{})
		return err
	}
	if err := c.app.CheckNodeName(ctx, name); err != nil {
		text := "Node name could not be checked. Try again."
		if errors.Is(err, ErrDuplicateNodeName) {
			text = "A Node with that name already exists. Enter another name."
		}
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, text, Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingName, func(current *wizard) {
		current.nodeName = name
		current.state = stateAwaitingIP
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Enter the public VPS IP address.", Keyboard{})
	return err
}

func (c *Controller) acceptVPSIP(ctx context.Context, message *Message, session *wizard) error {
	address, valid := parsePublicIP(message.Text)
	if !valid {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Invalid VPS address. Enter a publicly routable IPv4 or IPv6 address.", Keyboard{})
		return err
	}
	if err := c.app.CheckVPSAddress(ctx, address); err != nil {
		text := "VPS address could not be checked. Try again."
		if errors.Is(err, ErrDuplicateVPSIP) {
			text = "A Node with that VPS address already exists. Enter another address."
		}
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, text, Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingIP, func(current *wizard) {
		current.vpsIP = address
		current.state = stateAwaitingPassword
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Send the temporary root password. The message will be deleted immediately when possible.", Keyboard{})
	return err
}

func (c *Controller) acceptPassword(ctx context.Context, message *Message, session *wizard) error {
	password := []byte(message.Text)
	message.Text = ""
	_ = c.messenger.DeleteMessage(ctx, message.ChatID, message.ID)
	if len(password) == 0 || len(password) > maxPasswordBytes {
		clearBytes(password)
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "The password is empty or too long. Send it again.", Keyboard{})
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingPassword, func(current *wizard) {
		clearBytes(current.password)
		current.password = append([]byte(nil), password...)
		current.state = statePreflighting
	}) {
		clearBytes(password)
		return c.sendExpired(ctx, message.ChatID)
	}
	clearBytes(password)

	current, found, expired := c.loadSession(message.FromUserID)
	if expired || !found || current.state != statePreflighting {
		return c.sendExpired(ctx, message.ChatID)
	}
	requestPassword, copied := c.copyPassword(message.FromUserID, current.nonce, statePreflighting)
	if !copied {
		return c.sendExpired(ctx, message.ChatID)
	}
	result, err := c.app.Preflight(ctx, PreflightInput{
		OperatorUserID: current.userID,
		HostID:         current.selected.ID,
		NodeName:       current.nodeName,
		VPSIP:          current.vpsIP,
		Password:       requestPassword,
	})
	clearBytes(requestPassword)
	result.PreparedDeploymentID = strings.TrimSpace(result.PreparedDeploymentID)
	if err != nil || strings.TrimSpace(result.PreparedDeploymentID) == "" {
		if strings.TrimSpace(result.PreparedDeploymentID) != "" {
			_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		}
		removed := c.takeSession(message.FromUserID, current.nonce, statePreflighting)
		if removed != nil {
			removed.clear()
		}
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "VPS preflight failed. Review the VPS details and start again.", mainKeyboard())
		return sendErr
	}
	if !validReadiness(result.CertificateReadiness) {
		result.CertificateReadiness = ReadinessUnknown
	}
	if !validReadiness(result.ConfigProfileReadiness) {
		result.ConfigProfileReadiness = ReadinessUnknown
	}
	if !c.updateSession(message.FromUserID, current.nonce, statePreflighting, func(active *wizard) {
		active.preflight = result
		active.state = stateAwaitingConfirmation
	}) {
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		return c.sendExpired(ctx, message.ChatID)
	}
	current, _, _ = c.loadSession(message.FromUserID)
	sent, sendErr := c.messenger.SendMessage(ctx, message.ChatID, renderConfirmation(current), confirmationKeyboard(current.nonce))
	if sendErr != nil {
		removed := c.takeSession(message.FromUserID, current.nonce, stateAwaitingConfirmation)
		if removed != nil {
			removed.clear()
		}
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		return sendErr
	}
	if !c.updateSession(message.FromUserID, current.nonce, stateAwaitingConfirmation, func(active *wizard) {
		active.statusMsgID = sent.ID
	}) {
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		_ = c.messenger.EditMessage(ctx, message.ChatID, sent.ID, "This confirmation expired before it could be activated. Start again.", Keyboard{})
		return nil
	}
	return nil
}

func (c *Controller) startDeployment(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if session.state != stateAwaitingConfirmation || session.statusMsgID == 0 || callback.Message.ID != session.statusMsgID {
		return c.expiredCallback(ctx, callback)
	}
	active := c.takeSession(callback.FromUserID, session.nonce, stateAwaitingConfirmation)
	if active == nil {
		return c.expiredCallback(ctx, callback)
	}
	active.state = stateDeploying
	if err := c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, renderProgress(Progress{Step: "starting", Completed: 0, Total: 0, SafeMessage: "Deployment started"}), Keyboard{}); err != nil {
		preparedID := active.preflight.PreparedDeploymentID
		active.clear()
		_ = c.app.CancelDeployment(ctx, preparedID)
		return err
	}

	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		defer active.clear()
		input := DeploymentInput{
			PreparedDeploymentID: active.preflight.PreparedDeploymentID,
			OperatorUserID:       active.userID,
			HostID:               active.selected.ID,
			NodeName:             active.nodeName,
			VPSIP:                active.vpsIP,
			Password:             active.password,
		}
		err := c.app.StartDeployment(ctx, input, func(progress Progress) error {
			return c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, renderProgress(progress), Keyboard{})
		})
		if err != nil {
			_ = c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, "❌ Deployment failed. Check the safe deployment history for details.", Keyboard{})
			return
		}
		_ = c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, "✅ Deployment completed.", Keyboard{})
	}()
	return nil
}

func (c *Controller) cancelFromMessage(ctx context.Context, message *Message) error {
	session, found, _ := c.loadSession(message.FromUserID)
	if found {
		removed := c.takeSession(message.FromUserID, session.nonce, session.state)
		if removed != nil {
			preparedID := removed.preflight.PreparedDeploymentID
			removed.clear()
			if preparedID != "" {
				_ = c.app.CancelDeployment(ctx, preparedID)
			}
		}
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Conversation cancelled.", mainKeyboard())
	return err
}

func (c *Controller) cancelFromCallback(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if session.state != stateAwaitingConfirmation || session.statusMsgID != callback.Message.ID {
		return c.expiredCallback(ctx, callback)
	}
	removed := c.takeSession(callback.FromUserID, session.nonce, stateAwaitingConfirmation)
	if removed == nil {
		return c.expiredCallback(ctx, callback)
	}
	preparedID := removed.preflight.PreparedDeploymentID
	removed.clear()
	if preparedID != "" {
		_ = c.app.CancelDeployment(ctx, preparedID)
	}
	return c.messenger.EditMessage(ctx, callback.Message.ChatID, callback.Message.ID, "❌ Deployment cancelled.", Keyboard{})
}

func (c *Controller) showMainMenu(ctx context.Context, chatID int64) error {
	_, err := c.messenger.SendMessage(ctx, chatID, "Choose an action:", mainKeyboard())
	return err
}

func (c *Controller) showNodes(ctx context.Context, chatID int64) error {
	nodes, err := c.app.ListNodes(ctx)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, chatID, "Nodes are temporarily unavailable.", mainKeyboard())
		return sendErr
	}
	var builder strings.Builder
	builder.WriteString("📡 Nodes\n")
	if len(nodes) == 0 {
		builder.WriteString("No Nodes found.")
	}
	for _, node := range nodes {
		state := "offline"
		if node.Connected {
			state = "connected"
		}
		fmt.Fprintf(&builder, "\n• %s — %s — %s", safeLine(node.Name, 60), safeLine(node.Address, 80), state)
		if builder.Len() >= maxMessageBytes {
			builder.WriteString("\n…")
			break
		}
	}
	_, err = c.messenger.SendMessage(ctx, chatID, truncateUTF8(builder.String(), maxMessageBytes), mainKeyboard())
	return err
}

func (c *Controller) showDeployments(ctx context.Context, chatID int64) error {
	deployments, err := c.app.ListDeployments(ctx, 20)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, chatID, "Deployments are temporarily unavailable.", mainKeyboard())
		return sendErr
	}
	var builder strings.Builder
	builder.WriteString("📜 Deployments\n")
	if len(deployments) == 0 {
		builder.WriteString("No deployments found.")
	}
	for _, item := range deployments {
		updated := "unknown time"
		if !item.UpdatedAt.IsZero() {
			updated = item.UpdatedAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(&builder, "\n• %s — %s — %s", safeLine(item.NodeName, 60), safeLine(item.Status, 40), updated)
		if builder.Len() >= maxMessageBytes {
			builder.WriteString("\n…")
			break
		}
	}
	_, err = c.messenger.SendMessage(ctx, chatID, truncateUTF8(builder.String(), maxMessageBytes), mainKeyboard())
	return err
}

func (c *Controller) authorized(userID int64) bool {
	_, allowed := c.allowed[userID]
	return allowed
}

func (c *Controller) putSession(session *wizard) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if previous := c.sessions[session.userID]; previous != nil {
		previous.clear()
	}
	c.scheduleExpiryLocked(session)
	c.sessions[session.userID] = session
}

func (c *Controller) loadSession(userID int64) (*wizard, bool, bool) {
	c.mu.Lock()
	session := c.sessions[userID]
	if session == nil {
		c.mu.Unlock()
		return nil, false, false
	}
	if !c.now().Before(session.expiresAt) {
		preparedID := session.preflight.PreparedDeploymentID
		session.clear()
		delete(c.sessions, userID)
		c.mu.Unlock()
		c.cancelPrepared(preparedID)
		return nil, false, true
	}
	snapshot := session.clone()
	c.mu.Unlock()
	return snapshot, true, false
}

func (c *Controller) updateSession(userID int64, nonce string, state wizardState, update func(*wizard)) bool {
	c.mu.Lock()
	session := c.sessions[userID]
	if session == nil || session.nonce != nonce || session.state != state || !c.now().Before(session.expiresAt) {
		if session != nil && !c.now().Before(session.expiresAt) {
			preparedID := session.preflight.PreparedDeploymentID
			session.clear()
			delete(c.sessions, userID)
			c.mu.Unlock()
			c.cancelPrepared(preparedID)
			return false
		}
		c.mu.Unlock()
		return false
	}
	update(session)
	session.expiresAt = c.now().Add(c.ttl)
	c.scheduleExpiryLocked(session)
	c.mu.Unlock()
	return true
}

func (c *Controller) takeSession(userID int64, nonce string, state wizardState) *wizard {
	c.mu.Lock()
	session := c.sessions[userID]
	if session == nil || session.nonce != nonce || session.state != state || !c.now().Before(session.expiresAt) {
		if session != nil && !c.now().Before(session.expiresAt) {
			preparedID := session.preflight.PreparedDeploymentID
			session.clear()
			delete(c.sessions, userID)
			c.mu.Unlock()
			c.cancelPrepared(preparedID)
			return nil
		}
		c.mu.Unlock()
		return nil
	}
	delete(c.sessions, userID)
	if session.expiryTimer != nil {
		session.expiryTimer.Stop()
		session.expiryTimer = nil
	}
	c.mu.Unlock()
	return session
}

func (c *Controller) scheduleExpiryLocked(session *wizard) {
	if session.expiryTimer != nil {
		session.expiryTimer.Stop()
	}
	userID := session.userID
	nonce := session.nonce
	session.expiryTimer = time.AfterFunc(c.ttl, func() {
		c.mu.Lock()
		current := c.sessions[userID]
		if current == nil || current.nonce != nonce || c.now().Before(current.expiresAt) {
			c.mu.Unlock()
			return
		}
		preparedID := current.preflight.PreparedDeploymentID
		current.clear()
		delete(c.sessions, userID)
		c.mu.Unlock()
		c.cancelPrepared(preparedID)
	})
}

func (c *Controller) copyPassword(userID int64, nonce string, state wizardState) ([]byte, bool) {
	c.mu.Lock()
	session := c.sessions[userID]
	if session == nil || session.nonce != nonce || session.state != state || !c.now().Before(session.expiresAt) || len(session.password) == 0 {
		if session != nil && !c.now().Before(session.expiresAt) {
			preparedID := session.preflight.PreparedDeploymentID
			session.clear()
			delete(c.sessions, userID)
			c.mu.Unlock()
			c.cancelPrepared(preparedID)
			return nil, false
		}
		c.mu.Unlock()
		return nil, false
	}
	password := append([]byte(nil), session.password...)
	c.mu.Unlock()
	return password, true
}

func (c *Controller) cancelExisting(ctx context.Context, userID int64) {
	c.mu.Lock()
	session := c.sessions[userID]
	delete(c.sessions, userID)
	c.mu.Unlock()
	if session == nil {
		return
	}
	preparedID := session.preflight.PreparedDeploymentID
	session.clear()
	if preparedID != "" {
		_ = c.app.CancelDeployment(ctx, preparedID)
	}
}

func (c *Controller) cancelPrepared(deploymentID string) {
	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelTimeout)
	defer cancel()
	_ = c.app.CancelDeployment(ctx, deploymentID)
}

func (c *Controller) expiredCallback(ctx context.Context, callback *CallbackQuery) error {
	_ = c.messenger.AnswerCallback(ctx, callback.ID, "This action has expired.")
	_, err := c.messenger.SendMessage(ctx, callback.Message.ChatID, "This conversation is incomplete or expired. Start again from the menu.", mainKeyboard())
	return err
}

func (c *Controller) sendExpired(ctx context.Context, chatID int64) error {
	_, err := c.messenger.SendMessage(ctx, chatID, "This conversation has expired. Start again from the menu.", mainKeyboard())
	return err
}

func mainKeyboard() Keyboard {
	return Keyboard{Reply: [][]string{{MenuAddNode}, {MenuNodes, MenuDeployments}}}
}

func confirmationKeyboard(nonce string) Keyboard {
	return Keyboard{Inline: [][]Button{
		{{Text: "🚀 Deploy", CallbackData: "add:deploy:" + nonce}},
		{{Text: "❌ Cancel", CallbackData: "add:cancel:" + nonce}},
	}}
}

func renderConfirmation(session *wizard) string {
	certificate := readinessText(session.preflight.CertificateReadiness)
	profile := session.preflight.ConfigProfileReadiness
	if profile == ReadinessUnknown {
		profile = session.selected.ConfigProfileReadiness
	}
	dnsZone := "not resolved"
	if strings.TrimSpace(session.preflight.DNSZone) != "" {
		dnsZone = safeLine(session.preflight.DNSZone, 120)
	}
	var builder strings.Builder
	builder.WriteString("Confirm deployment\n")
	fmt.Fprintf(&builder, "Host: %s\n", safeLine(session.selected.Remark, 120))
	fmt.Fprintf(&builder, "SNI: %s\n", safeLine(session.selected.Address, 255))
	fmt.Fprintf(&builder, "Node name: %s\n", safeLine(session.nodeName, 60))
	fmt.Fprintf(&builder, "VPS IP: %s\n", session.vpsIP.String())
	fmt.Fprintf(&builder, "DNS zone: %s\n", dnsZone)
	fmt.Fprintf(&builder, "Certificate: %s\n", certificate)
	fmt.Fprintf(&builder, "Config profile: %s", readinessText(profile))
	for _, warning := range session.preflight.SafeWarnings {
		warning = safeLine(warning, 160)
		if warning != "" {
			fmt.Fprintf(&builder, "\nWarning: %s", warning)
		}
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func renderProgress(progress Progress) string {
	step := safeLine(progress.Step, 80)
	if step == "" {
		step = "working"
	}
	message := safeLine(progress.SafeMessage, 180)
	if message == "" {
		message = "In progress"
	}
	header := "🚀 Deployment in progress"
	if progress.Total > 0 && progress.Completed >= 0 && progress.Completed <= progress.Total {
		header = fmt.Sprintf("🚀 Deployment in progress (%d/%d)", progress.Completed, progress.Total)
	}
	return fmt.Sprintf("%s\nStep: %s\n%s", header, step, message)
}

func readinessText(value Readiness) string {
	switch value {
	case ReadinessReady:
		return "ready"
	case ReadinessNotReady:
		return "not ready"
	default:
		return "unknown"
	}
}

func validReadiness(value Readiness) bool {
	return value == "" || value == ReadinessUnknown || value == ReadinessReady || value == ReadinessNotReady
}

func parseCallbackData(data string) (action, nonce string, index int, valid bool) {
	parts := strings.Split(data, ":")
	if len(parts) < 3 || parts[0] != "add" || parts[2] == "" {
		return "", "", 0, false
	}
	switch parts[1] {
	case "host":
		if len(parts) != 4 {
			return "", "", 0, false
		}
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 {
			return "", "", 0, false
		}
		return "host", parts[2], parsed, true
	case "deploy", "cancel":
		if len(parts) != 3 {
			return "", "", 0, false
		}
		return parts[1], parts[2], 0, true
	default:
		return "", "", 0, false
	}
}

func randomNonce() (string, error) {
	var value [9]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value[:]), nil
}

func safeLine(value string, limit int) string {
	value = strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "\t", " ").Replace(value))
	return truncateUTF8(value, limit)
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	for len(value) > limit {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}
