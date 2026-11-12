package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const maxTelegramProfileInbounds = 80

func (c *Controller) beginProfileHost(ctx context.Context, message *Message) error {
	if _, ok := c.app.(ProfileHostApplication); !ok {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Создание Host из профиля недоступно.", mainKeyboard())
		return err
	}
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
		session := &wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingProfileHostInbound, expiresAt: c.now().Add(c.ttl), panels: panels, panel: panels[0], profileHost: true}
		c.putSession(session)
		return c.showProfileHostInbounds(ctx, session, 0)
	}
	session := &wizard{userID: message.FromUserID, chatID: message.ChatID, nonce: nonce, state: stateSelectingProfileHostPanel, expiresAt: c.now().Add(c.ttl), panels: panels, profileHost: true}
	c.putSession(session)
	rows := make([][]Button, 0, len(panels))
	for index, panel := range panels {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("ph:panel:%s:%d", nonce, index)}})
	}
	_, err = c.messenger.SendMessage(ctx, message.ChatID, "Выберите панель, профиль которой будет использован для нового Host:", Keyboard{Inline: rows})
	return err
}

func (c *Controller) selectProfileHostPanel(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingProfileHostPanel || index < 0 || index >= len(session.panels) {
		return c.expiredCallback(ctx, callback)
	}
	panel := session.panels[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostPanel, func(current *wizard) {
		current.panel = panel
		current.state = stateSelectingProfileHostInbound
	}) {
		return c.expiredCallback(ctx, callback)
	}
	current, _, _ := c.loadSession(session.userID)
	return c.showProfileHostInbounds(ctx, current, callback.Message.ID)
}

func (c *Controller) showProfileHostInbounds(ctx context.Context, session *wizard, messageID int) error {
	options, err := c.app.(ProfileHostApplication).ListProfileHostOptions(ctx, session.panel.ID)
	if err != nil {
		c.cancelExisting(ctx, session.userID)
		message := "Не удалось загрузить Config Profiles из Remnawave."
		var operatorError interface{ OperatorMessage() string }
		if errors.As(err, &operatorError) && strings.TrimSpace(operatorError.OperatorMessage()) != "" {
			message = operatorError.OperatorMessage()
		}
		_, sendErr := c.messenger.SendMessage(ctx, session.chatID, message, mainKeyboard())
		return sendErr
	}
	if len(options.Inbounds) == 0 {
		c.cancelExisting(ctx, session.userID)
		message := fmt.Sprintf("Профили получены, но подходящих inbound нет. Профилей: %d, inbound: %d, без корректного порта: %d.", options.TotalProfiles, options.TotalInbounds, options.SkippedWithoutPort)
		_, sendErr := c.messenger.SendMessage(ctx, session.chatID, message, mainKeyboard())
		return sendErr
	}
	visible := limitProfileHostInbounds(options.Inbounds)
	if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostInbound, func(current *wizard) {
		current.profileAllInbounds = cloneProfileHostInbounds(options.Inbounds)
		current.profileInbounds = cloneProfileHostInbounds(visible)
		current.profileSearchQuery = ""
	}) {
		return c.sendExpired(ctx, session.chatID)
	}
	current, found, expired := c.loadSession(session.userID)
	if expired || !found {
		return c.sendExpired(ctx, session.chatID)
	}
	return c.renderProfileHostInbounds(ctx, current, messageID, len(options.Inbounds))
}

func (c *Controller) renderProfileHostInbounds(ctx context.Context, session *wizard, messageID, total int) error {
	rows := make([][]Button, 0, len(session.profileInbounds)+1)
	for index, inbound := range session.profileInbounds {
		label := fmt.Sprintf("%s · %s · :%d", inbound.ProfileName, inbound.InboundTag, inbound.Port)
		rows = append(rows, []Button{{Text: safeLine(label, 60), CallbackData: fmt.Sprintf("ph:inbound:%s:%d", session.nonce, index)}})
	}
	actions := []Button{{Text: "🔎 Поиск", CallbackData: fmt.Sprintf("ph:search:%s", session.nonce)}}
	if session.profileSearchQuery != "" {
		actions = append(actions, Button{Text: "♻️ Сбросить", CallbackData: fmt.Sprintf("ph:reset:%s", session.nonce)})
	}
	rows = append(rows, actions)
	text := "Выберите существующий Config Profile и inbound. Порт Host будет взят из профиля."
	if session.profileSearchQuery != "" {
		text = fmt.Sprintf("🔎 Результаты по запросу «%s»: %d.", safeLine(session.profileSearchQuery, 100), total)
	}
	if total > len(session.profileInbounds) {
		text += fmt.Sprintf(" Показаны первые %d — уточните поиск.", len(session.profileInbounds))
	}
	if messageID != 0 {
		return c.messenger.EditMessage(ctx, session.chatID, messageID, text, Keyboard{Inline: rows})
	}
	_, err := c.messenger.SendMessage(ctx, session.chatID, text, Keyboard{Inline: rows})
	return err
}

func (c *Controller) beginProfileHostSearch(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if session.state != stateSelectingProfileHostInbound || len(session.profileAllInbounds) == 0 {
		return c.expiredCallback(ctx, callback)
	}
	if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostInbound, func(current *wizard) {
		current.state = stateAwaitingProfileHostSearch
		current.statusMsgID = callback.Message.ID
	}) {
		return c.expiredCallback(ctx, callback)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID,
		"🔎 Поиск Config Profile\n\nОтправьте часть названия профиля и/или serverName.",
		Keyboard{Inline: [][]Button{{{Text: "⬅️ Все профили", CallbackData: fmt.Sprintf("ph:reset:%s", session.nonce)}}}},
	)
}

func (c *Controller) acceptProfileHostSearch(ctx context.Context, message *Message, session *wizard) error {
	query := strings.TrimSpace(message.Text)
	if query == "" || len(query) > 100 {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Введите запрос длиной от 1 до 100 символов.", Keyboard{})
		return err
	}
	matches := searchProfileHostInbounds(session.profileAllInbounds, query)
	_ = c.messenger.DeleteMessage(ctx, message.ChatID, message.ID)
	if len(matches) == 0 {
		return c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID,
			fmt.Sprintf("🔎 По запросу «%s» ничего не найдено. Отправьте другой запрос.", safeLine(query, 100)),
			Keyboard{Inline: [][]Button{{{Text: "⬅️ Все профили", CallbackData: fmt.Sprintf("ph:reset:%s", session.nonce)}}}},
		)
	}
	visible := limitProfileHostInbounds(matches)
	if !c.updateSession(session.userID, session.nonce, stateAwaitingProfileHostSearch, func(current *wizard) {
		current.state = stateSelectingProfileHostInbound
		current.profileInbounds = cloneProfileHostInbounds(visible)
		current.profileSearchQuery = query
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	current, found, expired := c.loadSession(session.userID)
	if expired || !found {
		return c.sendExpired(ctx, message.ChatID)
	}
	return c.renderProfileHostInbounds(ctx, current, session.statusMsgID, len(matches))
}

func (c *Controller) resetProfileHostSearch(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if (session.state != stateSelectingProfileHostInbound && session.state != stateAwaitingProfileHostSearch) || len(session.profileAllInbounds) == 0 {
		return c.expiredCallback(ctx, callback)
	}
	visible := limitProfileHostInbounds(session.profileAllInbounds)
	if !c.updateSession(session.userID, session.nonce, session.state, func(current *wizard) {
		current.state = stateSelectingProfileHostInbound
		current.profileInbounds = cloneProfileHostInbounds(visible)
		current.profileSearchQuery = ""
	}) {
		return c.expiredCallback(ctx, callback)
	}
	current, found, expired := c.loadSession(session.userID)
	if expired || !found {
		return c.sendExpired(ctx, session.chatID)
	}
	return c.renderProfileHostInbounds(ctx, current, callback.Message.ID, len(current.profileAllInbounds))
}

func limitProfileHostInbounds(values []ProfileHostInbound) []ProfileHostInbound {
	if len(values) > maxTelegramProfileInbounds {
		values = values[:maxTelegramProfileInbounds]
	}
	return cloneProfileHostInbounds(values)
}

func searchProfileHostInbounds(values []ProfileHostInbound, query string) []ProfileHostInbound {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	if len(terms) == 0 {
		return nil
	}
	result := make([]ProfileHostInbound, 0)
	for _, inbound := range values {
		haystack := strings.ToLower(strings.TrimSpace(inbound.ProfileName))
		for _, serverName := range inbound.ServerNames {
			haystack += "\n" + strings.ToLower(strings.TrimSpace(serverName))
		}
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			result = append(result, inbound)
		}
	}
	return result
}

func (c *Controller) selectProfileHostInbound(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingProfileHostInbound || index < 0 || index >= len(session.profileInbounds) {
		return c.expiredCallback(ctx, callback)
	}
	selected := session.profileInbounds[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostInbound, func(current *wizard) {
		current.selectedProfile = selected
		current.state = stateSelectingProfileHostDomain
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := make([][]Button, 0, len(selected.ServerNames)+1)
	for domainIndex, domain := range selected.ServerNames {
		rows = append(rows, []Button{{Text: safeLine(domain, 60), CallbackData: fmt.Sprintf("ph:domain:%s:%d", session.nonce, domainIndex)}})
	}
	rows = append(rows, []Button{{Text: "✍️ Ввести домен вручную", CallbackData: fmt.Sprintf("ph:domain:%s:%d", session.nonce, len(selected.ServerNames))}})
	text := fmt.Sprintf("Профиль: %s\nInbound: %s\nПорт: %d\n\nВыберите serverNames из профиля или введите FQDN вручную.", safeLine(selected.ProfileName, 80), safeLine(selected.InboundTag, 80), selected.Port)
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, text, Keyboard{Inline: rows})
}

func (c *Controller) selectProfileHostDomain(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingProfileHostDomain || index < 0 || index > len(session.selectedProfile.ServerNames) {
		return c.expiredCallback(ctx, callback)
	}
	if index == len(session.selectedProfile.ServerNames) {
		if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostDomain, func(current *wizard) { current.state = stateAwaitingProfileHostDomain }) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Введите FQDN нового Host. Из-за ограничения Remnawave 2.8.1 имя должно быть не длиннее 40 символов.", Keyboard{})
	}
	domain := session.selectedProfile.ServerNames[index]
	if !c.updateSession(session.userID, session.nonce, stateSelectingProfileHostDomain, func(current *wizard) {
		current.profileDomain = domain
		current.state = stateAwaitingName
	}) {
		return c.expiredCallback(ctx, callback)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Host: "+domain+"\n\nВведите имя новой Node (3–30 символов).", Keyboard{})
}

func (c *Controller) acceptProfileHostDomain(ctx context.Context, message *Message, session *wizard) error {
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(message.Text), "."))
	if !validBalancerFQDN(domain) {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Некорректный FQDN. Нужен обычный домен длиной до 40 символов, без wildcard.", Keyboard{})
		return err
	}
	if !c.updateSession(session.userID, session.nonce, stateAwaitingProfileHostDomain, func(current *wizard) {
		current.profileDomain = domain
		current.state = stateAwaitingName
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	_, err := c.messenger.SendMessage(ctx, message.ChatID, "Host: "+domain+"\n\nВведите имя новой Node (3–30 символов).", Keyboard{})
	return err
}
