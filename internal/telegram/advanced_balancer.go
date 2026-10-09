package telegram

import (
	"context"
	"fmt"
	"strings"
)

func (c *Controller) beginAdvancedBalancer(ctx context.Context, message *Message) error {
	app, ok := c.app.(AdvancedBalancerApplication)
	if !ok {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Advanced Balancer API недоступен.", mainKeyboard())
		return err
	}
	_ = app
	c.cancelExisting(ctx, message.FromUserID)
	panels, err := c.app.ListPanels(ctx)
	if err != nil || len(panels) == 0 {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Панели временно недоступны.", mainKeyboard())
		return sendErr
	}
	nonce, err := c.nonce()
	if err != nil {
		return err
	}
	if len(panels) == 1 {
		session := &wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingBalancerSquad, expiresAt: c.now().Add(c.ttl), panels: panels, panel: panels[0], advancedBalancer: true}
		c.putSession(session)
		return c.showBalancerSquads(ctx, session, 0)
	}
	session := &wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingBalancerPanel, expiresAt: c.now().Add(c.ttl), panels: panels, advancedBalancer: true}
	c.putSession(session)
	rows := make([][]Button, 0, len(panels))
	for index, panel := range panels {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("bal:panel:%s:%d", nonce, index)}})
	}
	_, err = c.messenger.SendMessage(ctx, message.ChatID, "Выберите Remnawave-панель для нового балансировочного Host:", Keyboard{Inline: rows})
	return err
}

func (c *Controller) selectBalancerPanel(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingBalancerPanel || index < 0 || index >= len(session.panels) {
		return c.expiredCallback(ctx, callback)
	}
	panel := session.panels[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingBalancerPanel, func(current *wizard) { current.panel = panel; current.state = stateSelectingBalancerSquad }) {
		return c.expiredCallback(ctx, callback)
	}
	current, _, _ := c.loadSession(session.userID)
	return c.showBalancerSquads(ctx, current, callback.Message.ID)
}

func (c *Controller) showBalancerSquads(ctx context.Context, session *wizard, messageID int) error {
	options, err := c.app.(AdvancedBalancerApplication).ListAdvancedBalancerOptions(ctx, session.panel.ID)
	if err != nil || len(options.Squads) == 0 {
		_, sendErr := c.messenger.SendMessage(ctx, session.chatID, "Internal Squads недоступны.", mainKeyboard())
		return sendErr
	}
	if len(options.Tags) == 0 {
		c.cancelExisting(ctx, session.userID)
		_, sendErr := c.messenger.SendMessage(ctx, session.chatID, "В панели нет тегов подходящих Host. Нужен включённый видимый Host с тегом и выбранным шаблоном Xray JSON.", mainKeyboard())
		return sendErr
	}
	if !c.updateSession(session.userID, session.nonce, stateSelectingBalancerSquad, func(current *wizard) {
		current.squads = options.Squads
		current.balancerTags = options.Tags
	}) {
		return c.sendExpired(ctx, session.chatID)
	}
	rows := make([][]Button, 0, len(options.Squads))
	for index, squad := range options.Squads {
		rows = append(rows, []Button{{Text: safeLine(squad.Name, 48), CallbackData: fmt.Sprintf("bal:squad:%s:%d", session.nonce, index)}})
	}
	text := "Выберите Internal Squad. Новый inbound будет безопасно добавлен к уже существующим inbound этого сквада."
	if messageID != 0 {
		return c.messenger.EditMessage(ctx, session.chatID, messageID, text, Keyboard{Inline: rows})
	}
	_, err = c.messenger.SendMessage(ctx, session.chatID, text, Keyboard{Inline: rows})
	return err
}

func (c *Controller) selectBalancerSquad(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingBalancerSquad || index < 0 || index >= len(session.squads) {
		return c.expiredCallback(ctx, callback)
	}
	squad := session.squads[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingBalancerSquad, func(current *wizard) { current.selectedSquad = squad; current.state = stateSelectingBalancerTag }) {
		return c.expiredCallback(ctx, callback)
	}
	current, _, _ := c.loadSession(session.userID)
	rows := make([][]Button, 0, (len(current.balancerTags)+1)/2)
	for tagIndex, tag := range current.balancerTags {
		button := Button{Text: safeLine(tag, 36), CallbackData: fmt.Sprintf("bal:tag:%s:%d", current.nonce, tagIndex)}
		if tagIndex%2 == 0 {
			rows = append(rows, []Button{button})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], button)
		}
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выберите тег включённого видимого Host. Показаны только Host с настроенным шаблоном Xray JSON.", Keyboard{Inline: rows})
}

func (c *Controller) selectBalancerTag(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingBalancerTag || index < 0 || index >= len(session.balancerTags) {
		return c.expiredCallback(ctx, callback)
	}
	tag := session.balancerTags[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingBalancerTag, func(current *wizard) { current.balancerTag = tag; current.state = stateAwaitingBalancerFQDN }) {
		return c.expiredCallback(ctx, callback)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выбран тег: "+safeLine(tag, 36)+"\n\nВведите полный новый поддомен Host, например gmb.wey-land.net. Бот создаст для него отдельную A-запись напрямую в Cloudflare.", Keyboard{})
}

func (c *Controller) acceptBalancerFQDN(ctx context.Context, message *Message, session *wizard) error {
	fqdn := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(message.Text), "."))
	if !validBalancerFQDN(fqdn) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Некорректное доменное имя. Введите полный поддомен.", Keyboard{})
		return err
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerFQDN, func(current *wizard) { current.hostFQDN = fqdn; current.state = stateAwaitingBalancerNodeName }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Введите имя новой Node (3–30 символов).", Keyboard{})
	return err
}

func (c *Controller) acceptBalancerNodeName(ctx context.Context, message *Message, session *wizard) error {
	name := strings.TrimSpace(message.Text)
	if !validNodeName(name) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Некорректное имя Node.", Keyboard{})
		return err
	}
	if err := c.app.CheckNodeName(ctx, session.panel.ID, name); err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Имя уже занято или не удалось проверить. Введите другое имя.", Keyboard{})
		return sendErr
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerNodeName, func(current *wizard) { current.nodeName = name; current.state = stateSelectingBalancerIPMode }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Сколько публичных IP у сервера?", Keyboard{Inline: [][]Button{
		{{Text: "1 IP — вход и выход совпадают", CallbackData: "bal:ipmode:" + session.nonce + ":0"}},
		{{Text: "2 IP — отдельные вход и выход", CallbackData: "bal:ipmode:" + session.nonce + ":1"}},
	}})
	return err
}

func (c *Controller) selectBalancerIPMode(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingBalancerIPMode || index < 0 || index > 1 {
		return c.expiredCallback(ctx, callback)
	}
	if !c.updateSession(session.userID, session.nonce, stateSelectingBalancerIPMode, func(current *wizard) { current.dualIP = index == 1; current.state = stateAwaitingBalancerIngressIP }) {
		return c.expiredCallback(ctx, callback)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите входной публичный IP. Он будет адресом Node, SSH-адресом и A-записью поддомена.", Keyboard{})
}

func (c *Controller) acceptBalancerIngressIP(ctx context.Context, message *Message, session *wizard) error {
	address, valid := parsePublicIP(message.Text)
	if !valid || !address.Is4() {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Введите публичный IPv4 входного адреса.", Keyboard{})
		return err
	}
	if err := c.app.CheckVPSAddress(ctx, session.panel.ID, address); err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Этот IP уже занят Node или проверка недоступна.", Keyboard{})
		return sendErr
	}
	next := stateAwaitingBalancerPassword
	if session.dualIP {
		next = stateAwaitingBalancerEgressIP
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerIngressIP, func(current *wizard) { current.vpsIP = address; current.state = next }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	text := "Отправьте временный root-пароль. Сообщение будет сразу удалено."
	if session.dualIP {
		text = "Введите выходной публичный IPv4. Он попадёт только в outbound.sendThrough и будет виден интернету."
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, text, Keyboard{})
	return err
}

func (c *Controller) acceptBalancerEgressIP(ctx context.Context, message *Message, session *wizard) error {
	address, valid := parsePublicIP(message.Text)
	if !valid || !address.Is4() || address == session.vpsIP {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Введите другой публичный IPv4 выходного адреса.", Keyboard{})
		return err
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerEgressIP, func(current *wizard) { current.egressIP = address; current.state = stateAwaitingBalancerPassword }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Отправьте временный root-пароль. Сообщение будет сразу удалено.", Keyboard{})
	return err
}

func (c *Controller) acceptBalancerPassword(ctx context.Context, message *Message, session *wizard) error {
	password := []byte(message.Text)
	message.Text = ""
	_ = c.messenger.DeleteMessage(ctx, message.ChatID, message.ID)
	if len(password) == 0 || len(password) > maxPasswordBytes {
		clearBytes(password)
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Пароль пустой или слишком длинный.", Keyboard{})
		return err
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerPassword, func(current *wizard) {
		current.password = append(current.password[:0], password...)
		current.state = stateBalancerPreflighting
	}) {
		clearBytes(password)
		return c.sendExpired(ctx, message.ChatID)
	}
	clearBytes(password)
	current, found, expired := c.loadSession(session.userID)
	if expired || !found {
		return c.sendExpired(ctx, message.ChatID)
	}
	requestPassword, copied := c.copyPassword(session.userID, session.nonce, stateBalancerPreflighting)
	if !copied {
		return c.sendExpired(ctx, message.ChatID)
	}
	result, err := c.app.(AdvancedBalancerApplication).PreflightAdvancedBalancer(ctx, AdvancedBalancerPreflightInput{PanelID: current.panel.ID, OperatorUserID: current.userID, InternalSquadID: current.selectedSquad.ID, BalancerTag: current.balancerTag, HostFQDN: current.hostFQDN, NodeName: current.nodeName, IngressIP: current.vpsIP, EgressIP: current.egressIP, Password: requestPassword})
	clearBytes(requestPassword)
	if err != nil || strings.TrimSpace(result.PreparedDeploymentID) == "" {
		removed := c.takeSession(session.userID, session.nonce, stateBalancerPreflighting)
		if removed != nil {
			removed.clear()
		}
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Предварительная проверка не пройдена. Откройте список развёртываний для безопасного кода ошибки.", mainKeyboard())
		return sendErr
	}
	if !c.updateSession(session.userID, session.nonce, stateBalancerPreflighting, func(active *wizard) {
		active.preflight = result
		active.selected = Host{ID: result.PreparedDeploymentID, Remark: active.hostFQDN, Address: active.hostFQDN, ConfigProfileReadiness: ReadinessReady}
		clearBytes(active.password)
		active.password = nil
		active.state = stateAwaitingBalancerConfirmation
	}) {
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		return c.sendExpired(ctx, message.ChatID)
	}
	current, _, _ = c.loadSession(session.userID)
	sent, sendErr := c.messenger.SendMessage(ctx, message.ChatID, renderAdvancedBalancerConfirmation(current), advancedBalancerConfirmationKeyboard(session.nonce))
	if sendErr != nil {
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
		return sendErr
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingBalancerConfirmation, func(active *wizard) { active.statusMsgID = sent.ID }) {
		_ = c.app.CancelDeployment(ctx, result.PreparedDeploymentID)
	}
	return nil
}

func (c *Controller) cancelAdvancedBalancer(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if session.state != stateAwaitingBalancerConfirmation || session.statusMsgID != callback.Message.ID {
		return c.expiredCallback(ctx, callback)
	}
	removed := c.takeSession(session.userID, session.nonce, stateAwaitingBalancerConfirmation)
	if removed == nil {
		return c.expiredCallback(ctx, callback)
	}
	id := removed.preflight.PreparedDeploymentID
	removed.clear()
	if id != "" {
		_ = c.app.CancelDeployment(ctx, id)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Создание балансировочного Host отменено.", Keyboard{})
}

func advancedBalancerConfirmationKeyboard(nonce string) Keyboard {
	return Keyboard{Inline: [][]Button{{{Text: "🚀 Создать", CallbackData: "bal:deploy:" + nonce}}, {{Text: "❌ Отмена", CallbackData: "bal:cancel:" + nonce}}}}
}

func renderAdvancedBalancerConfirmation(session *wizard) string {
	mode := "1 IP: sendThrough отсутствует"
	egress := "совпадает с входным"
	if session.dualIP {
		mode = "2 IP: sendThrough включён"
		egress = session.egressIP.String()
	}
	return fmt.Sprintf("Подтвердите Advanced Balancer Host\nПанель: %s\nInternal Squad: %s\nТег: %s\nHost/FQDN: %s\nNode: %s\nВходной IP (Node, SSH, Cloudflare A): %s\nВыходной IP (интернет): %s\nРежим: %s\n\nБудут созданы отдельные Config Profile, Node, скрытый Host и inbound в Squad.", safeLine(session.panel.Name, 80), safeLine(session.selectedSquad.Name, 80), session.balancerTag, session.hostFQDN, safeLine(session.nodeName, 60), session.vpsIP, egress, mode)
}

func validBalancerFQDN(value string) bool {
	if len(value) < 4 || len(value) > 253 || !strings.Contains(value, ".") {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}
