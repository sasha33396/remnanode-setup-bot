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
	nodesPageSize     = 15
	legacyMenuAddNode = "➕ Add Node"
	legacyMenuNodes   = "📡 Nodes"
	legacyMenuDeploys = "📜 Deployments"
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
	case MenuAddNode, legacyMenuAddNode:
		return c.beginAddNode(ctx, message)
	case MenuNodes, legacyMenuNodes:
		c.cancelExisting(ctx, message.FromUserID)
		return c.showNodes(ctx, message.ChatID)
	case MenuDeployments, legacyMenuDeploys:
		c.cancelExisting(ctx, message.FromUserID)
		return c.showDeployments(ctx, message.ChatID)
	case MenuChangeIP:
		return c.beginIPChange(ctx, message)
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
	case stateSelectingPanel, stateSelectingIPPanel, stateSelectingServerIPPanel, stateSelectingDNSSyncPanel:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Выберите панель кнопкой.", Keyboard{})
		return err
	case stateSelectingIPMode, stateSelectingServerIPScope:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Выберите вариант смены IP кнопкой.", Keyboard{})
		return err
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
	case stateAwaitingIPChangeQuery:
		return c.acceptIPChangeQuery(ctx, message, session)
	case stateAwaitingIPChangeConfirmation:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Используйте кнопки «Сменить» или «Отменить» под карточкой ноды.", Keyboard{})
		return err
	case stateAwaitingNewIP:
		return c.acceptNewNodeIP(ctx, message, session)
	case stateAwaitingServerIPNodeQuery:
		return c.acceptServerIPNodeQuery(ctx, message, session)
	case stateAwaitingServerCurrentIP:
		return c.acceptServerCurrentIP(ctx, message, session)
	case stateAwaitingServerNewIP:
		return c.acceptServerNewIP(ctx, message, session)
	case stateAwaitingServerIPPassword:
		return c.acceptServerIPPassword(ctx, message, session)
	case stateAwaitingDNSSyncQuery:
		return c.acceptDNSSyncQuery(ctx, message, session)
	case stateAwaitingDNSSyncConfirmation:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Подтвердите синхронизацию кнопкой под карточкой.", Keyboard{})
		return err
	case stateDNSSyncRunning:
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "⏳ Синхронизация DNS уже выполняется.", Keyboard{})
		return err
	case stateDNSSyncCompleted:
		return c.deliverDNSSyncResult(ctx, session.chatID, session.statusMsgID, session.dnsSyncResult)
	default:
		return c.sendExpired(ctx, message.ChatID)
	}
}

func (c *Controller) handleRecoveryCommand(ctx context.Context, message *Message, text string) (bool, error) {
	if text == "/bootstrap_certificate" || strings.HasPrefix(text, "/bootstrap_certificate ") {
		fields := strings.Fields(text)
		if len(fields) != 3 || !strings.EqualFold(fields[2], "CONFIRM") {
			_, err := c.messenger.SendMessage(ctx, message.ChatID, "Usage: /bootstrap_certificate <sni-domain> CONFIRM", mainKeyboard())
			return true, err
		}
		recoveryApp, ok := c.app.(RecoveryApplication)
		if !ok {
			_, err := c.messenger.SendMessage(ctx, message.ChatID, "Recovery actions are unavailable.", mainKeyboard())
			return true, err
		}
		result, err := recoveryApp.BootstrapCertificate(ctx, fields[1], message.FromUserID)
		if err != nil {
			failure := "Certificate bootstrap could not be completed safely."
			if detail := safeLine(result, 300); detail != "" {
				failure += " " + detail
			}
			_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, failure, mainKeyboard())
			return true, sendErr
		}
		_, err = c.messenger.SendMessage(ctx, message.ChatID, safeLine(result, 500), mainKeyboard())
		return true, err
	}
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
		entries, err := recoveryApp.ViewSafeLogs(ctx, deploymentID)
		if err != nil {
			return true, c.sendActionResult(ctx, message.ChatID, err, "")
		}
		details, err := recoveryApp.GetDeploymentDetails(ctx, deploymentID)
		if err != nil {
			return true, c.sendActionResult(ctx, message.ChatID, err, "")
		}
		_, err = c.messenger.SendMessage(ctx, message.ChatID, renderDeploymentLogs(details, entries), mainKeyboard())
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
	if page, valid := parseNodesPageCallback(callback.Data); valid {
		_ = c.messenger.AnswerCallback(ctx, callback.ID, "")
		return c.showNodesPage(ctx, callback.Message.ChatID, callback.Message.ID, page)
	}
	if action, deploymentID, valid := parseDeploymentCallback(callback.Data); valid {
		return c.handleDeploymentCallback(ctx, callback, action, deploymentID)
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
	case "ip_mode":
		if session.state != stateSelectingIPMode {
			return c.expiredCallback(ctx, callback)
		}
		switch index {
		case 0:
			return c.beginPanelIPChange(ctx, callback, session)
		case 1:
			return c.beginServerIPScope(ctx, callback, session, serverIPProviderCherry)
		case 2:
			return c.beginServerIPScope(ctx, callback, session, serverIPProviderRoyal)
		case 3:
			return c.beginDNSSync(ctx, callback, session)
		default:
			return c.expiredCallback(ctx, callback)
		}
	case "server_scope":
		if session.state != stateSelectingServerIPScope {
			return c.expiredCallback(ctx, callback)
		}
		switch index {
		case 0:
			return c.beginServerNodeIPChange(ctx, callback, session)
		case 1:
			if !c.updateSession(callback.FromUserID, nonce, stateSelectingServerIPScope, func(current *wizard) {
				current.serverUpdateNode = false
				current.state = stateAwaitingServerCurrentIP
			}) {
				return c.expiredCallback(ctx, callback)
			}
			return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите текущий основной IPv4 сервера "+serverProviderName(session.serverIPProvider)+", по которому бот подключится по SSH.", Keyboard{})
		default:
			return c.expiredCallback(ctx, callback)
		}
	case "server_panel":
		if session.state != stateSelectingServerIPPanel || index < 0 || index >= len(session.panels) {
			return c.expiredCallback(ctx, callback)
		}
		if !c.updateSession(callback.FromUserID, nonce, stateSelectingServerIPPanel, func(current *wizard) {
			current.panel = session.panels[index]
			current.state = stateAwaitingServerIPNodeQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите точное имя ноды или её текущий IP-адрес.", Keyboard{})
	case "dns_panel":
		if session.state != stateSelectingDNSSyncPanel || index < 0 || index >= len(session.panels) {
			return c.expiredCallback(ctx, callback)
		}
		if !c.updateSession(callback.FromUserID, nonce, stateSelectingDNSSyncPanel, func(current *wizard) {
			current.panel = session.panels[index]
			current.state = stateAwaitingDNSSyncQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите точное имя ноды или её актуальный IP из Remnawave.", Keyboard{})
	case "panel":
		if session.state != stateSelectingPanel || index < 0 || index >= len(session.panels) {
			return c.expiredCallback(ctx, callback)
		}
		return c.showHostPicker(ctx, callback.FromUserID, session.chatID, session.nonce, session.panels[index])
	case "ip_panel":
		if session.state != stateSelectingIPPanel || index < 0 || index >= len(session.panels) {
			return c.expiredCallback(ctx, callback)
		}
		if !c.updateSession(callback.FromUserID, nonce, stateSelectingIPPanel, func(current *wizard) {
			current.panel = session.panels[index]
			current.state = stateAwaitingIPChangeQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		_, err := c.messenger.SendMessage(ctx, session.chatID, "Введите точное имя ноды или её текущий IP-адрес.", Keyboard{})
		return err
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
	case "ip_change":
		if session.state != stateAwaitingIPChangeConfirmation || session.statusMsgID != callback.Message.ID {
			return c.expiredCallback(ctx, callback)
		}
		if !c.updateSession(callback.FromUserID, nonce, stateAwaitingIPChangeConfirmation, func(current *wizard) {
			current.state = stateAwaitingNewIP
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, renderIPChangeTarget(session.ipTarget)+"\n\nВведите новый публичный IP-адрес.", Keyboard{})
	case "ip_cancel":
		if session.state != stateAwaitingIPChangeConfirmation || session.statusMsgID != callback.Message.ID {
			return c.expiredCallback(ctx, callback)
		}
		removed := c.takeSession(callback.FromUserID, nonce, stateAwaitingIPChangeConfirmation)
		if removed == nil {
			return c.expiredCallback(ctx, callback)
		}
		removed.clear()
		return c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, "Смена IP отменена.", Keyboard{})
	case "dns_sync":
		if session.statusMsgID != callback.Message.ID || !session.dnsSyncTarget.CanSync {
			return c.expiredCallback(ctx, callback)
		}
		if session.state == stateDNSSyncCompleted {
			return c.deliverDNSSyncResult(ctx, session.chatID, session.statusMsgID, session.dnsSyncResult)
		}
		if session.state == stateDNSSyncRunning {
			_, err := c.messenger.SendMessage(ctx, session.chatID, "⏳ Синхронизация DNS уже выполняется.", Keyboard{})
			return err
		}
		if session.state != stateAwaitingDNSSyncConfirmation || !c.updateSession(callback.FromUserID, nonce, stateAwaitingDNSSyncConfirmation, func(current *wizard) {
			current.state = stateDNSSyncRunning
		}) {
			return c.expiredCallback(ctx, callback)
		}
		_ = c.deliverDNSSyncResult(ctx, session.chatID, session.statusMsgID, "⏳ Синхронизирую DNS с актуальным IP из Remnawave…")
		result, err := c.app.(NodeDNSSyncApplication).SyncNodeDNS(ctx, NodeDNSSyncInput{PanelID: session.panel.ID, NodeUUID: session.dnsSyncTarget.UUID, ExpectedIP: session.dnsSyncTarget.Address})
		text := renderDNSSyncResult(result)
		if err != nil {
			text = "❌ Не удалось безопасно синхронизировать DNS. IP ноды в Remnawave не изменялся; запустите проверку заново."
		}
		if !c.updateSession(callback.FromUserID, nonce, stateDNSSyncRunning, func(current *wizard) {
			current.state = stateDNSSyncCompleted
			current.dnsSyncResult = text
		}) {
			return c.deliverDNSSyncResult(ctx, session.chatID, session.statusMsgID, text)
		}
		return c.deliverDNSSyncResult(ctx, session.chatID, session.statusMsgID, text)
	case "dns_cancel":
		if session.state != stateAwaitingDNSSyncConfirmation || session.statusMsgID != callback.Message.ID {
			return c.expiredCallback(ctx, callback)
		}
		removed := c.takeSession(callback.FromUserID, nonce, stateAwaitingDNSSyncConfirmation)
		if removed == nil {
			return c.expiredCallback(ctx, callback)
		}
		removed.clear()
		return c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, "Синхронизация DNS отменена.", Keyboard{})
	default:
		return c.expiredCallback(ctx, callback)
	}
}

func (c *Controller) beginAddNode(ctx context.Context, message *Message) error {
	c.cancelExisting(ctx, message.FromUserID)
	panels, err := c.app.ListPanels(ctx)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Панели временно недоступны.", mainKeyboard())
		return sendErr
	}
	if len(panels) == 0 {
		_, err = c.messenger.SendMessage(ctx, message.ChatID, "Нет доступных панелей.", mainKeyboard())
		return err
	}
	nonce, err := c.nonce()
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Could not start the wizard. Try again.", mainKeyboard())
		return errors.Join(errors.New("generate Telegram wizard nonce"), sendErr)
	}
	if len(panels) == 1 {
		return c.showHostPicker(ctx, message.FromUserID, message.ChatID, nonce, panels[0])
	}
	session := &wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingPanel, expiresAt: c.now().Add(c.ttl), panels: panels}
	c.putSession(session)
	rows := make([][]Button, 0, len(panels))
	for index, panel := range panels {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("add:panel:%s:%d", nonce, index)}})
	}
	_, err = c.messenger.SendMessage(ctx, message.ChatID, "Выберите Remnawave-панель:", Keyboard{Inline: rows})
	return err
}

func (c *Controller) showHostPicker(ctx context.Context, userID, chatID int64, nonce string, panel Panel) error {
	hosts, err := c.app.ListHosts(ctx, panel.ID)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, chatID, "Hosts are temporarily unavailable. Try again later.", mainKeyboard())
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
		_, err = c.messenger.SendMessage(ctx, chatID, "No deployable Hosts are available.", mainKeyboard())
		return err
	}
	session := &wizard{
		userID:    userID,
		chatID:    chatID,
		nonce:     nonce,
		state:     stateSelectingHost,
		expiresAt: c.now().Add(c.ttl),
		hosts:     selectable,
		panel:     panel,
	}
	c.putSession(session)

	rows := make([][]Button, 0, len(selectable))
	for index, host := range selectable {
		rows = append(rows, []Button{{
			Text:         safeLine(host.Remark, 48),
			CallbackData: fmt.Sprintf("add:host:%s:%d", nonce, index),
		}})
	}
	_, err = c.messenger.SendMessage(ctx, chatID, "Панель: "+safeLine(panel.Name, 80)+"\nВыберите Remnawave Host:", Keyboard{Inline: rows})
	return err
}

func (c *Controller) beginIPChange(ctx context.Context, message *Message) error {
	c.cancelExisting(ctx, message.FromUserID)
	_, nodeAvailable := c.app.(NodeIPApplication)
	_, cherryAvailable := c.app.(CherryIPApplication)
	_, royalAvailable := c.app.(RoyalIPApplication)
	_, dnsSyncAvailable := c.app.(NodeDNSSyncApplication)
	if !nodeAvailable && !cherryAvailable && !royalAvailable && !dnsSyncAvailable {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Смена IP сейчас недоступна.", mainKeyboard())
		return err
	}
	nonce, err := c.nonce()
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Не удалось начать смену IP. Повторите позже.", mainKeyboard())
		return errors.Join(err, sendErr)
	}
	rows := make([][]Button, 0, 4)
	if nodeAvailable {
		rows = append(rows, []Button{{Text: "Панель + DNS-балансировка", CallbackData: fmt.Sprintf("ip:mode:%s:%d", nonce, 0)}})
	}
	if cherryAvailable {
		rows = append(rows, []Button{{Text: "Смена IP на Cherry (сервер)", CallbackData: fmt.Sprintf("ip:mode:%s:%d", nonce, 1)}})
	}
	if royalAvailable {
		rows = append(rows, []Button{{Text: "Смена IP на Royal (сервер)", CallbackData: fmt.Sprintf("ip:mode:%s:%d", nonce, 2)}})
	}
	if dnsSyncAvailable {
		rows = append(rows, []Button{{Text: "🔄 Синхронизировать Remna → DNS", CallbackData: fmt.Sprintf("ip:mode:%s:%d", nonce, 3)}})
	}
	c.putSession(&wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingIPMode, expiresAt: c.now().Add(c.ttl)})
	_, err = c.messenger.SendMessage(ctx, message.ChatID, "Что именно нужно изменить?", Keyboard{Inline: rows})
	return err
}

func (c *Controller) beginDNSSync(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if _, ok := c.app.(NodeDNSSyncApplication); !ok {
		return c.expiredCallback(ctx, callback)
	}
	panels, err := c.app.ListPanels(ctx)
	if err != nil {
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Панели временно недоступны.", Keyboard{})
	}
	enabled := make([]Panel, 0, len(panels))
	for _, panel := range panels {
		if panel.DNSEnabled {
			enabled = append(enabled, panel)
		}
	}
	if len(enabled) == 0 {
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "DNS-балансировка отключена для всех панелей.", Keyboard{})
	}
	if len(enabled) == 1 {
		if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
			current.panel = enabled[0]
			current.state = stateAwaitingDNSSyncQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите точное имя ноды или её актуальный IP из Remnawave.", Keyboard{})
	}
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
		current.panels = enabled
		current.state = stateSelectingDNSSyncPanel
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := make([][]Button, 0, len(enabled))
	for index, panel := range enabled {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("dns:panel:%s:%d", session.nonce, index)}})
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выберите панель, из которой взять титульный IP:", Keyboard{Inline: rows})
}

func (c *Controller) beginServerIPScope(ctx context.Context, callback *CallbackQuery, session *wizard, provider serverIPProvider) error {
	if !c.serverProviderAvailable(provider) {
		return c.expiredCallback(ctx, callback)
	}
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
		current.serverIPProvider = provider
		current.state = stateSelectingServerIPScope
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := [][]Button{
		{{Text: "Нода уже в Remnawave", CallbackData: fmt.Sprintf("ip:scope:%s:%d", session.nonce, 0)}},
		{{Text: "Нода ещё не добавлена", CallbackData: fmt.Sprintf("ip:scope:%s:%d", session.nonce, 1)}},
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Добавлена ли эта нода в Remnawave?", Keyboard{Inline: rows})
}

func (c *Controller) beginPanelIPChange(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if _, ok := c.app.(NodeIPApplication); !ok {
		return c.expiredCallback(ctx, callback)
	}
	panels, err := c.app.ListPanels(ctx)
	if err != nil || len(panels) == 0 {
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Панели временно недоступны.", Keyboard{})
	}
	if len(panels) == 1 {
		if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
			current.panel = panels[0]
			current.state = stateAwaitingIPChangeQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите точное имя ноды или её текущий IP-адрес.", Keyboard{})
	}
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
		current.panels = panels
		current.state = stateSelectingIPPanel
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := make([][]Button, 0, len(panels))
	for index, panel := range panels {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("ip:panel:%s:%d", session.nonce, index)}})
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выберите Remnawave-панель:", Keyboard{Inline: rows})
}

func (c *Controller) beginServerNodeIPChange(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if _, ok := c.app.(NodeIPApplication); !ok {
		button := Button{Text: "Нода ещё не добавлена", CallbackData: fmt.Sprintf("ip:scope:%s:%d", session.nonce, 1)}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Поиск нод Remnawave сейчас недоступен. Укажите IP сервера вручную.", Keyboard{Inline: [][]Button{{button}}})
	}
	panels, err := c.app.ListPanels(ctx)
	if err != nil || len(panels) == 0 {
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Панели временно недоступны.", Keyboard{})
	}
	if len(panels) == 1 {
		if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingServerIPScope, func(current *wizard) {
			current.panel = panels[0]
			current.serverUpdateNode = true
			current.state = stateAwaitingServerIPNodeQuery
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите точное имя ноды или её текущий IP-адрес.", Keyboard{})
	}
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingServerIPScope, func(current *wizard) {
		current.panels = panels
		current.serverUpdateNode = true
		current.state = stateSelectingServerIPPanel
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := make([][]Button, 0, len(panels))
	for index, panel := range panels {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("ip:spanel:%s:%d", session.nonce, index)}})
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выберите Remnawave-панель:", Keyboard{Inline: rows})
}

func (c *Controller) acceptIPChangeQuery(ctx context.Context, message *Message, session *wizard) error {
	target, err := c.app.(NodeIPApplication).FindNodeForIPChange(ctx, session.panel.ID, strings.TrimSpace(message.Text))
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Нода не найдена или запрос неоднозначен. Проверьте имя/IP и повторите.", Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingIPChangeQuery, func(current *wizard) {
		current.ipTarget = target
		current.state = stateAwaitingIPChangeConfirmation
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	sent, err := c.messenger.SendMessage(ctx, message.ChatID, renderIPChangeTarget(target), ipChangeKeyboard(session.nonce))
	if err != nil {
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingIPChangeConfirmation, func(current *wizard) { current.statusMsgID = sent.ID }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	return nil
}

func (c *Controller) acceptNewNodeIP(ctx context.Context, message *Message, session *wizard) error {
	newIP, valid := parsePublicIP(message.Text)
	if !valid {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Неверный IP. Введите публичный IPv4 или IPv6.", Keyboard{})
		return err
	}
	active := c.takeSession(message.FromUserID, session.nonce, stateAwaitingNewIP)
	if active == nil {
		return c.sendExpired(ctx, message.ChatID)
	}
	defer active.clear()
	result, err := c.app.(NodeIPApplication).ReplaceNodeIP(ctx, NodeIPChangeInput{PanelID: active.panel.ID, NodeUUID: active.ipTarget.UUID, ExpectedIP: active.ipTarget.Address, NewIP: newIP})
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Не удалось безопасно сменить IP. Данные ноды могли измениться; запустите смену IP заново.", mainKeyboard())
		return sendErr
	}
	_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "✅ "+safeLine(result, 400), mainKeyboard())
	return sendErr
}

func (c *Controller) acceptDNSSyncQuery(ctx context.Context, message *Message, session *wizard) error {
	target, err := c.app.(NodeDNSSyncApplication).FindNodeForDNSSync(ctx, session.panel.ID, strings.TrimSpace(message.Text))
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Не удалось проверить ноду и DNS. Проверьте точное имя/IP и доступность DNS-балансировки.", Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingDNSSyncQuery, func(current *wizard) {
		current.dnsSyncTarget = target
		current.state = stateAwaitingDNSSyncConfirmation
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	keyboard := dnsSyncKeyboard(session.nonce, target.CanSync)
	sent, err := c.messenger.SendMessage(ctx, message.ChatID, renderDNSSyncTarget(target), keyboard)
	if err != nil {
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingDNSSyncConfirmation, func(current *wizard) { current.statusMsgID = sent.ID }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	return nil
}

func (c *Controller) acceptServerCurrentIP(ctx context.Context, message *Message, session *wizard) error {
	address, valid := parsePublicIPv4(message.Text)
	if !valid {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Неверный адрес. Введите текущий публичный IPv4 сервера "+serverProviderName(session.serverIPProvider)+".", Keyboard{})
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingServerCurrentIP, func(current *wizard) {
		current.serverCurrentIP = address
		current.state = stateAwaitingServerNewIP
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, serverNewIPPrompt(session.serverIPProvider, false), Keyboard{})
	return err
}

func (c *Controller) acceptServerIPNodeQuery(ctx context.Context, message *Message, session *wizard) error {
	target, err := c.app.(NodeIPApplication).FindNodeForIPChange(ctx, session.panel.ID, strings.TrimSpace(message.Text))
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Нода не найдена или запрос неоднозначен. Проверьте имя/IP и повторите.", Keyboard{})
		return sendErr
	}
	serverIP := target.Address.Unmap()
	if !serverIP.IsValid() || !serverIP.Is4() {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Для "+serverProviderName(session.serverIPProvider)+" нужен текущий публичный IPv4 ноды.", Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingServerIPNodeQuery, func(current *wizard) {
		current.ipTarget = target
		current.serverCurrentIP = serverIP
		current.serverUpdateNode = true
		current.state = stateAwaitingServerNewIP
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	text := renderIPChangeTarget(target) + "\n\n" + serverNewIPPrompt(session.serverIPProvider, true)
	_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, text, Keyboard{})
	return sendErr
}

func (c *Controller) acceptServerNewIP(ctx context.Context, message *Message, session *wizard) error {
	address, valid := parsePublicIPv4(message.Text)
	if !valid || address == session.serverCurrentIP {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Неверный новый IP. Он должен быть публичным IPv4 и отличаться от текущего адреса сервера.", Keyboard{})
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingServerNewIP, func(current *wizard) {
		current.serverNewIP = address
		current.state = stateAwaitingServerIPPassword
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Отправьте временный root-пароль. Сообщение будет сразу удалено; пароль не сохраняется.", Keyboard{})
	return err
}

func (c *Controller) acceptServerIPPassword(ctx context.Context, message *Message, session *wizard) error {
	password := []byte(message.Text)
	message.Text = ""
	_ = c.messenger.DeleteMessage(ctx, message.ChatID, message.ID)
	if len(password) == 0 || len(password) > maxPasswordBytes {
		clearBytes(password)
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Пароль пустой или слишком длинный. Отправьте его ещё раз.", Keyboard{})
		return err
	}
	active := c.takeSession(message.FromUserID, session.nonce, stateAwaitingServerIPPassword)
	if active == nil {
		clearBytes(password)
		return c.sendExpired(ctx, message.ChatID)
	}
	active.password = append(active.password[:0], password...)
	clearBytes(password)
	statusText := serverStatusText(active.serverIPProvider)
	if active.serverUpdateNode {
		statusText += " После этого обновлю ноду и DNS."
	}
	status, err := c.messenger.SendMessage(ctx, message.ChatID, statusText, Keyboard{})
	if err != nil {
		active.clear()
		return err
	}
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		defer active.clear()
		text, configureErr := c.configureServerIP(ctx, active)
		if configureErr != nil {
			_ = c.messenger.EditMessage(ctx, active.chatID, status.ID, serverFailureText(active.serverIPProvider), Keyboard{})
			return
		}
		if active.serverUpdateNode {
			panelResult, replaceErr := c.app.(NodeIPApplication).ReplaceNodeIP(ctx, NodeIPChangeInput{
				PanelID:    active.panel.ID,
				NodeUUID:   active.ipTarget.UUID,
				ExpectedIP: active.ipTarget.Address,
				NewIP:      active.serverNewIP,
			})
			if replaceErr != nil {
				text += "\n\n⚠️ IP на сервере настроен, но ноду и DNS обновить не удалось. Запустите «Панель + DNS-балансировка» отдельно для этой ноды."
				_ = c.messenger.EditMessage(ctx, active.chatID, status.ID, text, Keyboard{})
				return
			}
			text += "\n\n✅ " + safeLine(panelResult, 400)
		}
		_ = c.messenger.EditMessage(ctx, active.chatID, status.ID, text, Keyboard{})
	}()
	return nil
}

func (c *Controller) serverProviderAvailable(provider serverIPProvider) bool {
	switch provider {
	case serverIPProviderCherry:
		_, ok := c.app.(CherryIPApplication)
		return ok
	case serverIPProviderRoyal:
		_, ok := c.app.(RoyalIPApplication)
		return ok
	default:
		return false
	}
}

func serverProviderName(provider serverIPProvider) string {
	if provider == serverIPProviderRoyal {
		return "Royal"
	}
	return "Cherry"
}

func serverNewIPPrompt(provider serverIPProvider, updateNode bool) string {
	text := "Введите новый floating IPv4, уже назначенный этому серверу в Cherry Servers."
	if provider == serverIPProviderRoyal {
		text = "Введите новый основной публичный IPv4 сервера Royal. Бот сохранит текущую маску и установит IPv4-шлюз x.x.x.1."
	}
	if updateNode {
		text += " После проверки SSH по новому адресу бот также обновит ноду и DNS."
	}
	return text
}

func serverStatusText(provider serverIPProvider) string {
	if provider == serverIPProviderRoyal {
		return "Подключаюсь к Royal-серверу, заменяю IPv4 и шлюз в netplan, затем проверяю SSH по новому адресу… Общий лимит — 90 секунд."
	}
	return "Подключаюсь к Cherry-серверу и настраиваю floating IP… Обычно это занимает меньше минуты, общий лимит — 90 секунд."
}

func serverFailureText(provider serverIPProvider) string {
	if provider == serverIPProviderRoyal {
		return "❌ Не удалось подтвердить смену IP на Royal-сервере. Нода и DNS не изменялись. Проверьте SSH по старому и новому IP, назначение адреса у провайдера и /var/log/remnanode-royal-netplan.log."
	}
	return "❌ Не удалось настроить IP на Cherry-сервере. Проверьте root-пароль, доступность SSH и назначение floating IP."
}

func (c *Controller) configureServerIP(ctx context.Context, active *wizard) (string, error) {
	switch active.serverIPProvider {
	case serverIPProviderCherry:
		result, err := c.app.(CherryIPApplication).ConfigureCherryIP(ctx, CherryIPInput{ServerIP: active.serverCurrentIP, FloatingIP: active.serverNewIP, Password: active.password})
		if err != nil {
			return "", err
		}
		persistent := "не сохранён в netplan"
		if result.Persistent {
			persistent = "сохранён в netplan"
		}
		text := fmt.Sprintf("✅ Floating IP %s настроен на %s\nИнтерфейс: %s\nПостоянная настройка: %s", active.serverNewIP, active.serverCurrentIP, safeLine(result.Interface, 64), persistent)
		if note := safeLine(result.PersistentNote, 500); note != "" {
			text += "\n" + note
		}
		return text, nil
	case serverIPProviderRoyal:
		result, err := c.app.(RoyalIPApplication).ConfigureRoyalIP(ctx, RoyalIPInput{ServerIP: active.serverCurrentIP, NewIP: active.serverNewIP, Password: active.password})
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("✅ IP Royal-сервера изменён: %s → %s/%d\nИнтерфейс: %s\nIPv4-шлюз: %s\nNetplan: %s\nРезервная копия: %s\nSSH по новому IP подтверждён", active.serverCurrentIP, active.serverNewIP, result.PrefixBits, safeLine(result.Interface, 64), result.Gateway, safeLine(result.NetplanFile, 160), safeLine(result.BackupFile, 180)), nil
	default:
		return "", errors.New("unknown server IP provider")
	}
}

func (c *Controller) acceptNodeName(ctx context.Context, message *Message, session *wizard) error {
	name := strings.TrimSpace(message.Text)
	if !validNodeName(name) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Invalid Node name. Use 3–30 printable characters.", Keyboard{})
		return err
	}
	if err := c.app.CheckNodeName(ctx, session.panel.ID, name); err != nil {
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
	if err := c.app.CheckVPSAddress(ctx, session.panel.ID, address); err != nil {
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
		PanelID:        current.panel.ID,
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
	progressView := newDeploymentProgressView(active.preflight.Warnings)
	if err := c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, progressView.Render(), Keyboard{}); err != nil {
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
			PanelID:              active.panel.ID,
			PreparedDeploymentID: active.preflight.PreparedDeploymentID,
			OperatorUserID:       active.userID,
			HostID:               active.selected.ID,
			NodeName:             active.nodeName,
			VPSIP:                active.vpsIP,
			Password:             active.password,
		}
		err := c.app.StartDeployment(ctx, input, func(progress Progress) error {
			progressView.Update(progress)
			return c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, progressView.Render(), Keyboard{})
		})
		if err != nil {
			progressView.Fail(err)
			_ = c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, progressView.Render(), Keyboard{})
			return
		}
		progressView.Complete()
		_ = c.messenger.EditMessage(ctx, active.chatID, active.statusMsgID, progressView.Render(), Keyboard{})
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
	_, err := c.messenger.SendMessage(ctx, chatID, "Выберите действие:", mainKeyboard())
	return err
}

func (c *Controller) showNodes(ctx context.Context, chatID int64) error {
	return c.showNodesPage(ctx, chatID, 0, 0)
}

func (c *Controller) showNodesPage(ctx context.Context, chatID int64, messageID, requestedPage int) error {
	nodes, err := c.app.ListNodes(ctx)
	if err != nil {
		if messageID != 0 {
			return c.messenger.EditMessage(ctx, chatID, messageID, "Ноды временно недоступны.", Keyboard{})
		}
		_, sendErr := c.messenger.SendMessage(ctx, chatID, "Ноды временно недоступны.", mainKeyboard())
		return sendErr
	}
	totalPages := (len(nodes) + nodesPageSize - 1) / nodesPageSize
	if totalPages == 0 {
		totalPages = 1
	}
	page := requestedPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}
	start := page * nodesPageSize
	end := start + nodesPageSize
	if end > len(nodes) {
		end = len(nodes)
	}

	var builder strings.Builder
	fmt.Fprintf(&builder, "📡 Ноды — страница %d из %d\n", page+1, totalPages)
	if len(nodes) == 0 {
		builder.WriteString("Ноды не найдены.")
	}
	for _, node := range nodes[start:end] {
		state := "не в сети"
		if node.Connected {
			state = "подключена"
		}
		fmt.Fprintf(&builder, "\n• [%s] %s — %s — %s", safeLine(node.PanelName, 40), safeLine(node.Name, 60), safeLine(node.Address, 80), state)
	}
	keyboard := nodesPageKeyboard(page, totalPages)
	text := truncateUTF8(builder.String(), maxMessageBytes)
	if messageID != 0 {
		return c.messenger.EditMessage(ctx, chatID, messageID, text, keyboard)
	}
	_, err = c.messenger.SendMessage(ctx, chatID, text, keyboard)
	return err
}

func (c *Controller) showDeployments(ctx context.Context, chatID int64) error {
	deployments, err := c.app.ListDeployments(ctx, 15)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, chatID, "Развёртывания временно недоступны.", mainKeyboard())
		return sendErr
	}
	rows := make([][]Button, 0, len(deployments))
	for _, item := range deployments {
		rows = append(rows, []Button{{Text: deploymentListButton(item), CallbackData: "dep:open:" + item.ID}})
	}
	_, err = c.messenger.SendMessage(ctx, chatID, renderDeploymentsList(deployments), Keyboard{Inline: rows})
	return err
}

func (c *Controller) handleDeploymentCallback(ctx context.Context, callback *CallbackQuery, action, deploymentID string) error {
	recoveryApp, ok := c.app.(RecoveryApplication)
	if !ok {
		return c.messenger.AnswerCallback(ctx, callback.ID, "Действия восстановления недоступны.")
	}
	_ = c.messenger.AnswerCallback(ctx, callback.ID, "")
	if action == "open" {
		return c.showDeploymentCard(ctx, recoveryApp, callback.Message.ChatID, callback.Message.ID, deploymentID, "")
	}
	details, err := recoveryApp.GetDeploymentDetails(ctx, deploymentID)
	if err != nil {
		return c.messenger.EditMessage(ctx, callback.Message.ChatID, callback.Message.ID, "Развёртывание больше недоступно.", Keyboard{})
	}
	switch action {
	case "logs":
		entries, logsErr := recoveryApp.ViewSafeLogs(ctx, deploymentID)
		if logsErr != nil {
			return c.showDeploymentCard(ctx, recoveryApp, callback.Message.ChatID, callback.Message.ID, deploymentID, "❌ Логи временно недоступны.")
		}
		return c.messenger.EditMessage(ctx, callback.Message.ChatID, callback.Message.ID, renderDeploymentLogs(details, entries), deploymentLogsKeyboard(deploymentID))
	case "retry", "dns", "recheck", "repair_cert", "cancel":
		if action == "repair_cert" && (!details.CanRepairCert || strings.TrimSpace(details.SNI) == "") {
			return c.showDeploymentCard(ctx, recoveryApp, callback.Message.ChatID, callback.Message.ID, deploymentID, "❌ Исправление сертификата неприменимо к текущему состоянию.")
		}
		if err := c.messenger.EditMessage(ctx, callback.Message.ChatID, callback.Message.ID, "⏳ Выполняю безопасное восстановление…", Keyboard{}); err != nil {
			return err
		}
		c.workers.Add(1)
		go c.runDeploymentAction(ctx, recoveryApp, callback, action, deploymentID, details)
		return nil
	default:
		return c.expiredCallback(ctx, callback)
	}
}

func (c *Controller) runDeploymentAction(ctx context.Context, app RecoveryApplication, callback *CallbackQuery, action, deploymentID string, details DeploymentDetails) {
	defer c.workers.Done()
	var result string
	var err error
	switch action {
	case "retry":
		err = app.RetryFailedStep(ctx, deploymentID)
	case "dns":
		err = app.RetryDNS(ctx, deploymentID)
	case "recheck":
		result, err = app.RecheckRemnawave(ctx, deploymentID)
	case "repair_cert":
		result, err = app.BootstrapCertificate(ctx, details.SNI, callback.FromUserID)
		if err == nil {
			err = app.RetryFailedStep(ctx, deploymentID)
			if err == nil {
				result = safeLine(result, 350) + "\nРазвёртывание продолжено."
			}
		}
	case "cancel":
		err = c.app.CancelDeployment(ctx, deploymentID)
	}
	message := "✅ Действие выполнено."
	if result != "" {
		message = "✅ " + safeLine(result, 500)
	}
	if err != nil {
		message = "❌ Действие не выполнено безопасно. Откройте логи этой карточки."
	}
	_ = c.showDeploymentCard(ctx, app, callback.Message.ChatID, callback.Message.ID, deploymentID, message)
}

func (c *Controller) showDeploymentCard(ctx context.Context, app RecoveryApplication, chatID int64, messageID int, deploymentID, notice string) error {
	details, err := app.GetDeploymentDetails(ctx, deploymentID)
	if err != nil {
		return c.messenger.EditMessage(ctx, chatID, messageID, "Развёртывание больше недоступно.", Keyboard{})
	}
	return c.messenger.EditMessage(ctx, chatID, messageID, renderDeploymentCard(details, notice), deploymentActionsKeyboard(details))
}

func deploymentActionsKeyboard(details DeploymentDetails) Keyboard {
	rows := [][]Button{{{Text: "📋 Подробный журнал", CallbackData: "dep:logs:" + details.ID}}}
	if details.CanRepairCert {
		rows = append(rows, []Button{{Text: "🛠 Исправить сертификат и продолжить", CallbackData: "dep:repair_cert:" + details.ID}})
	}
	if details.CanRetryDNS {
		rows = append(rows, []Button{{Text: "🔁 Повторить DNS", CallbackData: "dep:dns:" + details.ID}})
	}
	if details.CanRetryStep && !details.CanRepairCert {
		rows = append(rows, []Button{{Text: "🔁 Повторить шаг", CallbackData: "dep:retry:" + details.ID}})
	}
	if details.CanRecheck {
		rows = append(rows, []Button{{Text: "🔎 Перепроверить ноду", CallbackData: "dep:recheck:" + details.ID}})
	}
	if details.CanCancel {
		rows = append(rows, []Button{{Text: "❌ Отменить развёртывание", CallbackData: "dep:cancel:" + details.ID}})
	}
	return Keyboard{Inline: rows}
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
	return Keyboard{Reply: [][]string{{MenuAddNode, MenuChangeIP}, {MenuNodes, MenuDeployments}}}
}

func nodesPageKeyboard(page, totalPages int) Keyboard {
	if totalPages <= 1 {
		return mainKeyboard()
	}
	row := make([]Button, 0, 2)
	if page > 0 {
		row = append(row, Button{Text: "⬅️ Назад", CallbackData: fmt.Sprintf("nodes:%d", page-1)})
	}
	if page+1 < totalPages {
		row = append(row, Button{Text: "Вперёд ➡️", CallbackData: fmt.Sprintf("nodes:%d", page+1)})
	}
	return Keyboard{Inline: [][]Button{row}}
}

func ipChangeKeyboard(nonce string) Keyboard {
	return Keyboard{Inline: [][]Button{{
		{Text: "🔄 Сменить", CallbackData: "ip:change:" + nonce},
		{Text: "❌ Отменить", CallbackData: "ip:cancel:" + nonce},
	}}}
}

func dnsSyncKeyboard(nonce string, canSync bool) Keyboard {
	if !canSync {
		return Keyboard{Inline: [][]Button{{{Text: "Закрыть", CallbackData: "dns:cancel:" + nonce}}}}
	}
	return Keyboard{Inline: [][]Button{{
		{Text: "✅ Синхронизировать", CallbackData: "dns:sync:" + nonce},
		{Text: "❌ Отменить", CallbackData: "dns:cancel:" + nonce},
	}}}
}

func renderDNSSyncTarget(target NodeDNSSyncTarget) string {
	status := "не подключена"
	if target.Connected {
		status = "подключена"
	}
	kind := "legacy"
	if target.Managed {
		kind = "создана этим ботом"
	} else if target.DNSZone != "" {
		kind = "legacy, зона определена по профилю + inbound"
	}
	zone := "не определена"
	if target.DNSZone != "" {
		zone = target.DNSZone
	}
	presence := "⚠️ актуальный IP отсутствует в целевой DNS-зоне"
	if target.CurrentPresent {
		presence = "✅ актуальный IP есть в целевой DNS-зоне"
	}
	if !target.Managed && len(target.CurrentZones) != 0 {
		presence = "✅ IP найден в DNS-зонах: " + strings.Join(target.CurrentZones, ", ")
	}
	var builder strings.Builder
	builder.WriteString("🔄 Синхронизация Remna → DNS\n")
	fmt.Fprintf(&builder, "Панель: %s\n", safeLine(target.PanelName, 80))
	fmt.Fprintf(&builder, "Нода: %s\n", safeLine(target.Name, 80))
	fmt.Fprintf(&builder, "Титульный IP (Remna): %s\n", target.Address)
	fmt.Fprintf(&builder, "Статус: %s\n", status)
	fmt.Fprintf(&builder, "Тип: %s\n", kind)
	fmt.Fprintf(&builder, "Целевая DNS-зона: %s\n", safeLine(zone, 255))
	if target.PreviousIP.IsValid() && target.PreviousIP != target.Address {
		fmt.Fprintf(&builder, "IP в истории бота: %s\n", target.PreviousIP)
	}
	fmt.Fprintf(&builder, "Проверка: %s\n", safeLine(presence, 800))
	if target.Managed && !target.CurrentPresent && len(target.CurrentZones) != 0 {
		fmt.Fprintf(&builder, "Актуальный IP также найден в: %s\n", safeLine(strings.Join(target.CurrentZones, ", "), 800))
	}
	if target.Note != "" {
		fmt.Fprintf(&builder, "Действие: %s", safeLine(target.Note, 500))
	}
	if !target.CanSync {
		builder.WriteString("\n\nАвтоматическая запись отключена: целевую DNS-зону или формат записи нельзя определить безопасно.")
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func renderDNSSyncResult(result NodeDNSSyncResult) string {
	action := "DNS уже соответствовал Remnawave"
	switch result.Action {
	case "ADDED":
		action = "актуальный IP добавлен в DNS"
	case "REPLACED":
		action = "устаревший IP заменён актуальным"
	}
	return fmt.Sprintf("✅ Синхронизация завершена\nНода: %s\nТитульный IP (Remna): %s\nDNS-зона: %s\nРезультат: %s", safeLine(result.NodeName, 80), result.Address, safeLine(result.DNSZone, 255), action)
}

// deliverDNSSyncResult first updates the callback card without an unsupported
// reply keyboard. If Telegram rejects the edit, a separate message guarantees
// that the operator still receives the durable outcome.
func (c *Controller) deliverDNSSyncResult(ctx context.Context, chatID int64, messageID int, text string) error {
	if err := c.messenger.EditMessage(ctx, chatID, messageID, text, Keyboard{}); err == nil {
		return nil
	}
	_, err := c.messenger.SendMessage(ctx, chatID, text, mainKeyboard())
	return err
}

func renderIPChangeTarget(target NodeIPChangeTarget) string {
	status := "не подключена"
	if target.Connected {
		status = "подключена"
	}
	kind := "legacy"
	if target.IsManaged {
		kind = "создана этим ботом"
	}
	zones := "не найдены"
	if !target.DNSEnabled {
		zones = "отключена для этой панели"
	} else if len(target.DNSZones) > 0 {
		zones = strings.Join(target.DNSZones, ", ")
	}
	return fmt.Sprintf("Панель: %s\nНода: %s\nТекущий IP: %s\nСтатус: %s\nТип: %s\nDNS-зоны: %s", safeLine(target.PanelName, 80), safeLine(target.Name, 80), target.Address, status, kind, safeLine(zones, 800))
}

func confirmationKeyboard(nonce string) Keyboard {
	return Keyboard{Inline: [][]Button{
		{{Text: "🚀 Развернуть", CallbackData: "add:deploy:" + nonce}},
		{{Text: "❌ Отмена", CallbackData: "add:cancel:" + nonce}},
	}}
}

func renderConfirmation(session *wizard) string {
	certificate := readinessText(session.preflight.CertificateReadiness)
	profile := session.preflight.ConfigProfileReadiness
	if profile == ReadinessUnknown {
		profile = session.selected.ConfigProfileReadiness
	}
	dnsZone := "not resolved"
	if !session.panel.DNSEnabled {
		dnsZone = "disabled for this panel"
	} else if strings.TrimSpace(session.preflight.DNSZone) != "" {
		dnsZone = safeLine(session.preflight.DNSZone, 120)
	}
	var builder strings.Builder
	builder.WriteString("Confirm deployment\n")
	fmt.Fprintf(&builder, "Host: %s\n", safeLine(session.selected.Remark, 120))
	fmt.Fprintf(&builder, "Panel: %s\n", safeLine(session.panel.Name, 80))
	fmt.Fprintf(&builder, "SNI: %s\n", safeLine(session.selected.Address, 255))
	fmt.Fprintf(&builder, "Node name: %s\n", safeLine(session.nodeName, 60))
	fmt.Fprintf(&builder, "VPS IP: %s\n", session.vpsIP.String())
	fmt.Fprintf(&builder, "DNS zone: %s\n", dnsZone)
	fmt.Fprintf(&builder, "Certificate: %s\n", certificate)
	fmt.Fprintf(&builder, "Config profile: %s", readinessText(profile))
	for _, warning := range session.preflight.Warnings {
		message := safeLine(warning.Message, 160)
		if message != "" {
			fmt.Fprintf(&builder, "\n⚠️ [%s] (%s)", normalizeWarningCode(warning.Code), message)
		}
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
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
	if len(parts) < 3 || parts[2] == "" {
		return "", "", 0, false
	}
	if parts[0] == "ip" && len(parts) == 4 && parts[1] == "panel" {
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 {
			return "", "", 0, false
		}
		return "ip_panel", parts[2], parsed, true
	}
	if parts[0] == "ip" && len(parts) == 4 && parts[1] == "spanel" {
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 {
			return "", "", 0, false
		}
		return "server_panel", parts[2], parsed, true
	}
	if parts[0] == "ip" && len(parts) == 4 && parts[1] == "scope" {
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 || parsed > 1 {
			return "", "", 0, false
		}
		return "server_scope", parts[2], parsed, true
	}
	if parts[0] == "ip" && len(parts) == 4 && parts[1] == "mode" {
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 || parsed > 3 {
			return "", "", 0, false
		}
		return "ip_mode", parts[2], parsed, true
	}
	if parts[0] == "dns" && len(parts) == 4 && parts[1] == "panel" {
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 {
			return "", "", 0, false
		}
		return "dns_panel", parts[2], parsed, true
	}
	if parts[0] == "dns" && len(parts) == 3 {
		switch parts[1] {
		case "sync":
			return "dns_sync", parts[2], 0, true
		case "cancel":
			return "dns_cancel", parts[2], 0, true
		}
	}
	if parts[0] == "ip" && len(parts) == 3 {
		switch parts[1] {
		case "change":
			return "ip_change", parts[2], 0, true
		case "cancel":
			return "ip_cancel", parts[2], 0, true
		}
	}
	if parts[0] != "add" {
		return "", "", 0, false
	}
	switch parts[1] {
	case "host", "panel":
		if len(parts) != 4 {
			return "", "", 0, false
		}
		parsed, err := strconv.Atoi(parts[3])
		if err != nil || parsed < 0 {
			return "", "", 0, false
		}
		return parts[1], parts[2], parsed, true
	case "deploy", "cancel":
		if len(parts) != 3 {
			return "", "", 0, false
		}
		return parts[1], parts[2], 0, true
	default:
		return "", "", 0, false
	}
}

func parseDeploymentCallback(data string) (action, deploymentID string, valid bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "dep" || !validDeploymentID(parts[2]) {
		return "", "", false
	}
	switch parts[1] {
	case "open", "logs", "retry", "dns", "recheck", "repair_cert", "cancel":
		return parts[1], parts[2], true
	default:
		return "", "", false
	}
}

func parseNodesPageCallback(data string) (int, bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 2 || parts[0] != "nodes" {
		return 0, false
	}
	page, err := strconv.Atoi(parts[1])
	return page, err == nil && page >= 0
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
