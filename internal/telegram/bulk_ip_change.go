package telegram

import (
	"context"
	"fmt"
	"strings"
)

func (c *Controller) beginBulkIPChange(ctx context.Context, callback *CallbackQuery, session *wizard) error {
	if _, ok := c.app.(BulkNodeIPApplication); !ok {
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
			current.state = stateAwaitingBulkIPList
		}) {
			return c.expiredCallback(ctx, callback)
		}
		return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, bulkIPListPrompt(enabled[0]), Keyboard{})
	}
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingIPMode, func(current *wizard) {
		current.panels = enabled
		current.state = stateSelectingBulkIPPanel
	}) {
		return c.expiredCallback(ctx, callback)
	}
	rows := make([][]Button, 0, len(enabled))
	for index, panel := range enabled {
		rows = append(rows, []Button{{Text: safeLine(panel.Name, 48), CallbackData: fmt.Sprintf("bulk:panel:%s:%d", session.nonce, index)}})
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, "Выберите Remnawave-панель для массовой смены IP:", Keyboard{Inline: rows})
}

func (c *Controller) selectBulkIPPanel(ctx context.Context, callback *CallbackQuery, session *wizard, index int) error {
	if session.state != stateSelectingBulkIPPanel || index < 0 || index >= len(session.panels) {
		return c.expiredCallback(ctx, callback)
	}
	panel := session.panels[index]
	if !c.updateSession(callback.FromUserID, session.nonce, stateSelectingBulkIPPanel, func(current *wizard) {
		current.panel = panel
		current.state = stateAwaitingBulkIPList
	}) {
		return c.expiredCallback(ctx, callback)
	}
	return c.messenger.EditMessage(ctx, session.chatID, callback.Message.ID, bulkIPListPrompt(panel), Keyboard{})
}

func bulkIPListPrompt(panel Panel) string {
	return "Панель: " + safeLine(panel.Name, 80) + "\n\nОтправьте список замен, по одной на строку:\n\n1.1.1.1 -> 2.2.2.2\n3.3.3.3 -> 4.4.4.4\n\nСтарый IP должен находиться ровно у одной ноды панели. Максимум 50 замен."
}

func (c *Controller) acceptBulkIPList(ctx context.Context, message *Message, session *wizard) error {
	mappings, err := parseBulkNodeIPMappings(message.Text)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, safeLine(err.Error(), 300)+"\n\nИсправьте список и отправьте его повторно.", Keyboard{})
		return sendErr
	}
	plan, err := c.app.(BulkNodeIPApplication).PrepareBulkNodeIPChange(ctx, session.panel.ID, mappings)
	if err != nil {
		_, sendErr := c.messenger.SendMessage(ctx, message.ChatID, "Не удалось безопасно подготовить массовую смену IP. Проверьте, что каждый старый IP принадлежит ровно одной ноде, новые IP свободны и список не содержит перестановок адресов.", Keyboard{})
		return sendErr
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingBulkIPList, func(current *wizard) {
		current.bulkIPPlan = cloneBulkNodeIPPlan(plan)
		current.state = stateAwaitingBulkIPConfirmation
	}) {
		return c.sendExpired(ctx, message.ChatID)
	}
	sent, err := c.messenger.SendMessage(ctx, message.ChatID, renderBulkNodeIPPlan(session.panel, plan), bulkNodeIPConfirmationKeyboard(session.nonce, plan.DisabledCount))
	if err != nil {
		return err
	}
	if !c.updateSession(message.FromUserID, session.nonce, stateAwaitingBulkIPConfirmation, func(current *wizard) { current.statusMsgID = sent.ID }) {
		return c.sendExpired(ctx, message.ChatID)
	}
	return nil
}

func (c *Controller) startBulkIPChange(ctx context.Context, callback *CallbackQuery, session *wizard, enableDisabled, retry bool) error {
	expectedState := stateAwaitingBulkIPConfirmation
	if retry {
		expectedState = stateBulkIPCompleted
		enableDisabled = session.bulkEnableNodes
	}
	if session.state != expectedState || session.statusMsgID != callback.Message.ID {
		return c.expiredCallback(ctx, callback)
	}
	items := append([]BulkNodeIPPlanItem(nil), session.bulkIPPlan.Items...)
	if retry {
		items = retryableBulkNodeIPItems(session.bulkIPResult)
	}
	if len(items) == 0 {
		return c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, "Все элементы массовой смены IP уже выполнены.", Keyboard{})
	}
	if !c.updateSession(callback.FromUserID, session.nonce, expectedState, func(current *wizard) {
		current.bulkEnableNodes = enableDisabled
		current.state = stateBulkIPRunning
	}) {
		return c.expiredCallback(ctx, callback)
	}
	_ = c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, renderBulkNodeIPProgress(0, len(items), ""), Keyboard{})

	runCtx := context.WithoutCancel(ctx)
	c.workers.Add(1)
	go func() {
		defer c.workers.Done()
		application := c.app.(BulkNodeIPApplication)
		result, applyErr := application.ApplyBulkNodeIPChange(runCtx, BulkNodeIPApplyInput{
			PanelID: session.panel.ID, Items: items, EnableDisabled: enableDisabled,
		}, func(update BulkNodeIPProgress) {
			if update.Completed == update.Total || update.Completed%5 == 0 {
				_ = c.messenger.EditMessage(runCtx, session.chatID, session.statusMsgID, renderBulkNodeIPProgress(update.Completed, update.Total, update.Item.Plan.NodeName), Keyboard{})
			}
		})
		if applyErr != nil && len(result.Items) == 0 {
			if active := c.takeSession(session.userID, session.nonce, stateBulkIPRunning); active != nil {
				active.clear()
			}
			_ = c.messenger.EditMessage(runCtx, session.chatID, session.statusMsgID, "❌ Массовая смена IP остановлена до выполнения изменений. Повторите операцию из меню.", Keyboard{})
			return
		}
		text := renderBulkNodeIPResult(result, enableDisabled)
		if result.Warnings != 0 || result.Failed != 0 {
			if c.updateSession(session.userID, session.nonce, stateBulkIPRunning, func(current *wizard) {
				current.bulkIPResult = cloneBulkNodeIPResult(result)
				current.state = stateBulkIPCompleted
			}) {
				_ = c.messenger.EditMessage(runCtx, session.chatID, session.statusMsgID, text, bulkNodeIPResultKeyboard(session.nonce))
				return
			}
			_ = c.messenger.EditMessage(runCtx, session.chatID, session.statusMsgID, text, Keyboard{})
			return
		}
		if active := c.takeSession(session.userID, session.nonce, stateBulkIPRunning); active != nil {
			active.clear()
		}
		_ = c.messenger.EditMessage(runCtx, session.chatID, session.statusMsgID, text, Keyboard{})
	}()
	return nil
}

func retryableBulkNodeIPItems(result BulkNodeIPResult) []BulkNodeIPPlanItem {
	items := make([]BulkNodeIPPlanItem, 0, result.Warnings+result.Failed)
	for _, item := range result.Items {
		if item.Status != BulkNodeIPCompleted {
			items = append(items, item.Plan)
		}
	}
	return items
}

func parseBulkNodeIPMappings(value string) ([]BulkNodeIPMapping, error) {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	result := make([]BulkNodeIPMapping, 0, len(lines))
	for lineIndex, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "->")
		if len(parts) != 2 {
			return nil, fmt.Errorf("Строка %d: используйте формат старый_IP -> новый_IP", lineIndex+1)
		}
		oldIP, oldOK := parsePublicIPv4(strings.TrimSpace(parts[0]))
		newIP, newOK := parsePublicIPv4(strings.TrimSpace(parts[1]))
		if !oldOK || !newOK || oldIP == newIP {
			return nil, fmt.Errorf("Строка %d: нужны два разных публичных IPv4", lineIndex+1)
		}
		result = append(result, BulkNodeIPMapping{OldIP: oldIP, NewIP: newIP})
		if len(result) > MaxBulkNodeIPChanges {
			return nil, fmt.Errorf("За один запуск допускается не более %d замен", MaxBulkNodeIPChanges)
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("Список замен пуст")
	}
	return result, nil
}

func renderBulkNodeIPPlan(panel Panel, plan BulkNodeIPPlan) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "📋 Массовая смена IP\nПанель: %s\nНод: %d\nОтключённых: %d\nБез найденной DNS-зоны: %d\n", safeLine(panel.Name, 80), len(plan.Items), plan.DisabledCount, plan.WithoutDNS)
	for index, item := range plan.Items {
		state := ""
		if item.WasDisabled {
			state = " · отключена"
		}
		zones := strings.Join(item.DNSZones, ", ")
		if zones == "" {
			zones = "не найдены"
		}
		fmt.Fprintf(&builder, "\n%d. %s%s\n%s → %s\nDNS: %s", index+1, safeLine(item.NodeName, 70), state, item.OldIP, item.NewIP, safeLine(zones, 180))
	}
	if plan.WithoutDNS != 0 {
		builder.WriteString("\n\n⚠️ Для нод без найденной DNS-зоны будет обновлён только Remnawave.")
	}
	if plan.DisabledCount != 0 {
		builder.WriteString("\n\nВыберите, включать ли отключённые ноды после успешного обновления IP и DNS.")
	} else {
		builder.WriteString("\n\nПодтвердите выполнение.")
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func bulkNodeIPConfirmationKeyboard(nonce string, disabled int) Keyboard {
	rows := make([][]Button, 0, 3)
	if disabled != 0 {
		rows = append(rows, []Button{{Text: "✅ Выполнить и включить отключённые", CallbackData: "bulk:run:" + nonce + ":1"}})
		rows = append(rows, []Button{{Text: "▶️ Выполнить, оставить отключёнными", CallbackData: "bulk:run:" + nonce + ":0"}})
	} else {
		rows = append(rows, []Button{{Text: "▶️ Выполнить", CallbackData: "bulk:run:" + nonce + ":0"}})
	}
	rows = append(rows, []Button{{Text: "❌ Отменить", CallbackData: "bulk:cancel:" + nonce}})
	return Keyboard{Inline: rows}
}

func bulkNodeIPResultKeyboard(nonce string) Keyboard {
	return Keyboard{Inline: [][]Button{
		{{Text: "🔁 Повторить ошибки", CallbackData: "bulk:retry:" + nonce}},
		{{Text: "Закрыть", CallbackData: "bulk:cancel:" + nonce}},
	}}
}

func renderBulkNodeIPProgress(completed, total int, nodeName string) string {
	text := fmt.Sprintf("⏳ Массовая смена IP выполняется\nВыполнено: %d/%d", completed, total)
	if strings.TrimSpace(nodeName) != "" {
		text += "\nПоследняя обработанная нода: " + safeLine(nodeName, 80)
	}
	return text
}

func renderBulkNodeIPResult(result BulkNodeIPResult, enableDisabled bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Массовая смена IP завершена\n\n✅ Выполнено: %d\n⚠️ Частично: %d\n❌ Ошибки: %d\n", result.Completed, result.Warnings, result.Failed)
	for _, item := range result.Items {
		icon := "✅"
		if item.Status == BulkNodeIPWarning {
			icon = "⚠️"
		} else if item.Status == BulkNodeIPFailed {
			icon = "❌"
		}
		fmt.Fprintf(&builder, "\n%s %s\n%s → %s", icon, safeLine(item.Plan.NodeName, 70), item.Plan.OldIP, item.Plan.NewIP)
		if item.SafeMessage != "" {
			fmt.Fprintf(&builder, "\n%s", safeLine(item.SafeMessage, 180))
		}
		if item.Plan.WasDisabled {
			switch {
			case !enableDisabled:
				builder.WriteString("\nВключение: оставлена отключённой")
			case item.EnableAttempted && item.Enabled && item.Connected:
				builder.WriteString("\nВключение: включена, подключена")
			case item.EnableAttempted && item.Enabled && item.Connecting:
				builder.WriteString("\nВключение: включена, подключается")
			case item.EnableAttempted && item.Enabled:
				builder.WriteString("\nВключение: включена, подключения пока нет")
			case item.EnableAttempted:
				builder.WriteString("\nВключение: не удалось")
			default:
				builder.WriteString("\nВключение: отложено до успешного обновления DNS")
			}
			if item.LastStatusMessage != "" {
				fmt.Fprintf(&builder, "\nСтатус ноды: %s", safeLine(item.LastStatusMessage, 120))
			}
		}
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}
