package telegram

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const nodeGroupPageSize = 12

type NodePolicy struct {
	CriticalFloor        int
	CriticalRatioPercent int
	CriticalCap          int
}

func DefaultNodePolicy() NodePolicy {
	return NodePolicy{CriticalFloor: 80, CriticalRatioPercent: 40, CriticalCap: 200}
}

func normalizeNodePolicy(policy NodePolicy) NodePolicy {
	defaults := DefaultNodePolicy()
	if policy.CriticalFloor < 1 {
		policy.CriticalFloor = defaults.CriticalFloor
	}
	if policy.CriticalRatioPercent < 1 || policy.CriticalRatioPercent > 100 {
		policy.CriticalRatioPercent = defaults.CriticalRatioPercent
	}
	if policy.CriticalCap < policy.CriticalFloor {
		policy.CriticalCap = defaults.CriticalCap
	}
	return policy
}

type nodeGroup string

const (
	nodeGroupCritical nodeGroup = "c"
	nodeGroupDisabled nodeGroup = "d"
	nodeGroupActive   nodeGroup = "a"
	nodeGroupIgnored  nodeGroup = "i"
)

type nodePanelSnapshot struct {
	Panel     Panel
	All       []NodeSummary
	Critical  []NodeSummary
	Disabled  []NodeSummary
	Active    []NodeSummary
	Ignored   []NodeSummary
	Baseline  int
	Threshold int
}

func classifyNodePanels(panels []Panel, nodes []NodeSummary, policy NodePolicy) []nodePanelSnapshot {
	policy = normalizeNodePolicy(policy)
	result := make([]nodePanelSnapshot, len(panels))
	byID := make(map[string]int, len(panels))
	for index, panel := range panels {
		result[index].Panel = panel
		byID[panel.ID] = index
	}
	for _, node := range nodes {
		index, found := byID[node.PanelID]
		if !found {
			continue
		}
		result[index].All = append(result[index].All, node)
		if node.Disabled {
			result[index].Disabled = append(result[index].Disabled, node)
		}
	}
	for index := range result {
		onlineValues := make([]int, 0, len(result[index].All))
		for _, node := range result[index].All {
			if !node.Disabled && node.Connected && node.OnlineKnown {
				onlineValues = append(onlineValues, node.Online)
			}
		}
		sort.Ints(onlineValues)
		result[index].Baseline = medianOnline(onlineValues)
		threshold := result[index].Baseline * policy.CriticalRatioPercent / 100
		if threshold < policy.CriticalFloor {
			threshold = policy.CriticalFloor
		}
		if threshold > policy.CriticalCap {
			threshold = policy.CriticalCap
		}
		result[index].Threshold = threshold
		for _, node := range result[index].All {
			switch {
			case node.Disabled:
				// Already classified above.
			case !node.Connected || !node.OnlineKnown:
				result[index].Ignored = append(result[index].Ignored, node)
			case node.Online < threshold:
				result[index].Critical = append(result[index].Critical, node)
			default:
				result[index].Active = append(result[index].Active, node)
			}
		}
		sort.Slice(result[index].Critical, func(a, b int) bool {
			if result[index].Critical[a].Online == result[index].Critical[b].Online {
				return result[index].Critical[a].Name < result[index].Critical[b].Name
			}
			return result[index].Critical[a].Online < result[index].Critical[b].Online
		})
		sort.Slice(result[index].Active, func(a, b int) bool {
			if result[index].Active[a].Online == result[index].Active[b].Online {
				return result[index].Active[a].Name < result[index].Active[b].Name
			}
			return result[index].Active[a].Online > result[index].Active[b].Online
		})
		sort.Slice(result[index].Disabled, func(a, b int) bool { return result[index].Disabled[a].Name < result[index].Disabled[b].Name })
		sort.Slice(result[index].Ignored, func(a, b int) bool { return result[index].Ignored[a].Name < result[index].Ignored[b].Name })
	}
	return result
}

func medianOnline(values []int) int {
	if len(values) == 0 {
		return 0
	}
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func (s nodePanelSnapshot) nodes(group nodeGroup) []NodeSummary {
	switch group {
	case nodeGroupCritical:
		return s.Critical
	case nodeGroupDisabled:
		return s.Disabled
	case nodeGroupActive:
		return s.Active
	case nodeGroupIgnored:
		return s.Ignored
	default:
		return nil
	}
}

func (c *Controller) nodeSnapshots(ctx context.Context) ([]nodePanelSnapshot, error) {
	panels, err := c.app.ListPanels(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := c.app.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	return classifyNodePanels(panels, nodes, c.nodePolicy), nil
}

func (c *Controller) showNodePanels(ctx context.Context, chatID int64, messageID int) error {
	snapshots, err := c.nodeSnapshots(ctx)
	if err != nil {
		return c.renderNodeInventoryFailure(ctx, chatID, messageID)
	}
	var builder strings.Builder
	builder.WriteString("📡 Ноды\nВыберите Remnawave-панель. Число на кнопке — общее количество нод.")
	rows := make([][]Button, 0, len(snapshots))
	for index, snapshot := range snapshots {
		rows = append(rows, []Button{{Text: fmt.Sprintf("%s — %d", safeLine(snapshot.Panel.Name, 42), len(snapshot.All)), CallbackData: fmt.Sprintf("nodes:p:%d", index)}})
	}
	if len(rows) == 0 {
		builder.WriteString("\nПанели не найдены.")
	}
	return c.sendOrEdit(ctx, chatID, messageID, builder.String(), Keyboard{Inline: rows})
}

func (c *Controller) showNodePanel(ctx context.Context, chatID int64, messageID, panelIndex int) error {
	snapshots, err := c.nodeSnapshots(ctx)
	if err != nil || panelIndex < 0 || panelIndex >= len(snapshots) {
		return c.renderNodeInventoryFailure(ctx, chatID, messageID)
	}
	snapshot := snapshots[panelIndex]
	text := fmt.Sprintf("📡 Ноды — %s\nВсего: %d\n🚨 Критический порог: менее %d онлайн\nОриентир панели (медиана): %d\n\nНоды без связи или без свежей метрики не участвуют в тревогах: %d.", safeLine(snapshot.Panel.Name, 80), len(snapshot.All), snapshot.Threshold, snapshot.Baseline, len(snapshot.Ignored))
	rows := [][]Button{
		{{Text: fmt.Sprintf("🚨 Критический онлайн — %d", len(snapshot.Critical)), CallbackData: fmt.Sprintf("nodes:g:%d:%s:0", panelIndex, nodeGroupCritical)}},
		{{Text: fmt.Sprintf("⏸ Отключённые — %d", len(snapshot.Disabled)), CallbackData: fmt.Sprintf("nodes:g:%d:%s:0", panelIndex, nodeGroupDisabled)}},
		{{Text: fmt.Sprintf("🟢 Активные / стабильные — %d", len(snapshot.Active)), CallbackData: fmt.Sprintf("nodes:g:%d:%s:0", panelIndex, nodeGroupActive)}},
		{{Text: "⬅️ К панелям", CallbackData: "nodes:root"}},
	}
	return c.sendOrEdit(ctx, chatID, messageID, text, Keyboard{Inline: rows})
}

func (c *Controller) showNodeGroup(ctx context.Context, chatID int64, messageID, panelIndex int, group nodeGroup, requestedPage int) error {
	snapshots, err := c.nodeSnapshots(ctx)
	if err != nil || panelIndex < 0 || panelIndex >= len(snapshots) {
		return c.renderNodeInventoryFailure(ctx, chatID, messageID)
	}
	snapshot := snapshots[panelIndex]
	nodes := snapshot.nodes(group)
	if nodes == nil {
		return c.renderNodeInventoryFailure(ctx, chatID, messageID)
	}
	totalPages := (len(nodes) + nodeGroupPageSize - 1) / nodeGroupPageSize
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
	start, end := page*nodeGroupPageSize, (page+1)*nodeGroupPageSize
	if end > len(nodes) {
		end = len(nodes)
	}
	label := nodeGroupLabel(group)
	text := fmt.Sprintf("%s — %s\nСтраница %d из %d. Нажмите на ноду, чтобы открыть карточку.", label, safeLine(snapshot.Panel.Name, 80), page+1, totalPages)
	rows := make([][]Button, 0, end-start+2)
	for _, node := range nodes[start:end] {
		rows = append(rows, []Button{{Text: nodeListButton(group, node), CallbackData: "nodes:o:" + node.UUID}})
	}
	if len(nodes) == 0 {
		text += "\nВ этой группе нод нет."
	}
	navigation := make([]Button, 0, 2)
	if page > 0 {
		navigation = append(navigation, Button{Text: "⬅️", CallbackData: fmt.Sprintf("nodes:g:%d:%s:%d", panelIndex, group, page-1)})
	}
	if page+1 < totalPages {
		navigation = append(navigation, Button{Text: "➡️", CallbackData: fmt.Sprintf("nodes:g:%d:%s:%d", panelIndex, group, page+1)})
	}
	if len(navigation) != 0 {
		rows = append(rows, navigation)
	}
	rows = append(rows, []Button{{Text: "⬅️ К группам", CallbackData: fmt.Sprintf("nodes:p:%d", panelIndex)}})
	return c.sendOrEdit(ctx, chatID, messageID, text, Keyboard{Inline: rows})
}

func (c *Controller) showNodeCard(ctx context.Context, chatID int64, messageID int, uuid string) error {
	snapshots, err := c.nodeSnapshots(ctx)
	if err != nil {
		return c.renderNodeInventoryFailure(ctx, chatID, messageID)
	}
	for panelIndex, snapshot := range snapshots {
		for _, node := range snapshot.All {
			if node.UUID != uuid {
				continue
			}
			group := nodeGroupIgnored
			for _, candidate := range snapshot.Critical {
				if candidate.UUID == uuid {
					group = nodeGroupCritical
				}
			}
			for _, candidate := range snapshot.Disabled {
				if candidate.UUID == uuid {
					group = nodeGroupDisabled
				}
			}
			for _, candidate := range snapshot.Active {
				if candidate.UUID == uuid {
					group = nodeGroupActive
				}
			}
			text := renderNodeCard(snapshot, node, group)
			rows := make([][]Button, 0, 3)
			if _, available := c.app.(NodeIPApplication); available {
				rows = append(rows, []Button{{Text: "🔄 Изменить IP ноды", CallbackData: "nodes:ip:" + node.UUID}})
			}
			rows = append(rows,
				[]Button{{Text: "🔄 Обновить", CallbackData: "nodes:o:" + node.UUID}},
				[]Button{{Text: "⬅️ К группам", CallbackData: fmt.Sprintf("nodes:p:%d", panelIndex)}},
			)
			keyboard := Keyboard{Inline: rows}
			return c.sendOrEdit(ctx, chatID, messageID, text, keyboard)
		}
	}
	return c.sendOrEdit(ctx, chatID, messageID, "Нода больше не найдена в настроенных панелях.", Keyboard{Inline: [][]Button{{{Text: "⬅️ К панелям", CallbackData: "nodes:root"}}}})
}

func renderNodeCard(snapshot nodePanelSnapshot, node NodeSummary, group nodeGroup) string {
	status := "без связи с панелью"
	if node.Disabled {
		status = "отключена"
	} else if node.Connected {
		status = "подключена"
	} else if node.Connecting {
		status = "подключается"
	}
	online := "нет свежей метрики"
	if node.OnlineKnown {
		online = strconv.Itoa(node.Online)
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\nПанель: %s\nНода: %s\nIP: %s\nСостояние: %s\nОнлайн: %s\nГруппа: %s", nodeGroupLabel(group), safeLine(node.PanelName, 80), safeLine(node.Name, 80), safeLine(node.Address, 80), status, online, nodeGroupDescription(group))
	if group == nodeGroupCritical || group == nodeGroupActive {
		fmt.Fprintf(&builder, "\nКритический порог панели: менее %d\nОриентир панели: %d", snapshot.Threshold, snapshot.Baseline)
	}
	if node.LastStatusChange != nil {
		fmt.Fprintf(&builder, "\nПоследнее изменение статуса: %s UTC", node.LastStatusChange.UTC().Format("2006-01-02 15:04:05"))
	}
	if message := safeLine(node.LastStatusMessage, 250); message != "" {
		fmt.Fprintf(&builder, "\nСообщение панели: %s", message)
	}
	if group == nodeGroupCritical {
		builder.WriteString("\n\n🚨 Рекомендация: проверьте блокировку публичного IP и запустите его замену.")
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func nodeListButton(group nodeGroup, node NodeSummary) string {
	switch group {
	case nodeGroupCritical:
		return fmt.Sprintf("🚨 %s — онлайн %d", safeLine(node.Name, 38), node.Online)
	case nodeGroupDisabled:
		return "⏸ " + safeLine(node.Name, 48)
	case nodeGroupActive:
		return fmt.Sprintf("🟢 %s — онлайн %d", safeLine(node.Name, 38), node.Online)
	default:
		return "⚪ " + safeLine(node.Name, 48)
	}
}

func nodeGroupLabel(group nodeGroup) string {
	switch group {
	case nodeGroupCritical:
		return "🚨 Критический онлайн"
	case nodeGroupDisabled:
		return "⏸ Отключённые ноды"
	case nodeGroupActive:
		return "🟢 Активные / стабильные ноды"
	default:
		return "⚪ Не участвует в контроле онлайна"
	}
}

func nodeGroupDescription(group nodeGroup) string {
	switch group {
	case nodeGroupCritical:
		return "высокий приоритет"
	case nodeGroupDisabled:
		return "средний приоритет"
	case nodeGroupActive:
		return "нормальная работа"
	default:
		return "без связи или без метрики; отдельные тревоги уже обрабатываются"
	}
}

func (c *Controller) sendOrEdit(ctx context.Context, chatID int64, messageID int, text string, keyboard Keyboard) error {
	if messageID != 0 {
		return c.messenger.EditMessage(ctx, chatID, messageID, truncateUTF8(text, maxMessageBytes), keyboard)
	}
	_, err := c.messenger.SendMessage(ctx, chatID, truncateUTF8(text, maxMessageBytes), keyboard)
	return err
}

func (c *Controller) renderNodeInventoryFailure(ctx context.Context, chatID int64, messageID int) error {
	return c.sendOrEdit(ctx, chatID, messageID, "Ноды или их онлайн-метрики временно недоступны.", Keyboard{Inline: [][]Button{{{Text: "🔄 Повторить", CallbackData: "nodes:root"}}}})
}

type nodesCallback struct {
	action     string
	panelIndex int
	group      nodeGroup
	page       int
	uuid       string
}

func parseNodesCallback(data string) (nodesCallback, bool) {
	parts := strings.Split(data, ":")
	if len(parts) == 2 && parts[0] == "nodes" && parts[1] == "root" {
		return nodesCallback{action: "root"}, true
	}
	if len(parts) == 3 && parts[0] == "nodes" && parts[1] == "p" {
		panel, err := strconv.Atoi(parts[2])
		return nodesCallback{action: "panel", panelIndex: panel}, err == nil && panel >= 0
	}
	if len(parts) == 5 && parts[0] == "nodes" && parts[1] == "g" {
		panel, panelErr := strconv.Atoi(parts[2])
		page, pageErr := strconv.Atoi(parts[4])
		group := nodeGroup(parts[3])
		validGroup := group == nodeGroupCritical || group == nodeGroupDisabled || group == nodeGroupActive
		return nodesCallback{action: "group", panelIndex: panel, group: group, page: page}, panelErr == nil && pageErr == nil && panel >= 0 && page >= 0 && validGroup
	}
	if len(parts) == 3 && parts[0] == "nodes" && parts[1] == "o" && validDeploymentID(parts[2]) {
		return nodesCallback{action: "open", uuid: parts[2]}, true
	}
	if len(parts) == 3 && parts[0] == "nodes" && parts[1] == "ip" && validDeploymentID(parts[2]) {
		return nodesCallback{action: "change_ip", uuid: parts[2]}, true
	}
	return nodesCallback{}, false
}

func (c *Controller) beginNodeCardIPChange(ctx context.Context, callback *CallbackQuery, uuid string) error {
	application, available := c.app.(NodeIPApplication)
	if !available {
		return c.sendOrEdit(ctx, callback.Message.ChatID, callback.Message.ID, "Смена IP ноды сейчас недоступна.", Keyboard{Inline: [][]Button{{{Text: "⬅️ К карточке", CallbackData: "nodes:o:" + uuid}}}})
	}
	nodes, err := c.app.ListNodes(ctx)
	if err != nil {
		return c.renderNodeIPStartFailure(ctx, callback, uuid)
	}
	var selected NodeSummary
	found := false
	for _, node := range nodes {
		if node.UUID == uuid {
			selected = node
			found = true
			break
		}
	}
	if !found || selected.PanelID == "" || selected.Name == "" {
		return c.renderNodeIPStartFailure(ctx, callback, uuid)
	}
	target, err := application.FindNodeForIPChange(ctx, selected.PanelID, selected.Name)
	if err != nil || target.UUID != uuid {
		return c.renderNodeIPStartFailure(ctx, callback, uuid)
	}
	nonce, err := c.nonce()
	if err != nil {
		return c.renderNodeIPStartFailure(ctx, callback, uuid)
	}
	c.cancelExisting(ctx, callback.FromUserID)
	session := &wizard{
		userID:      callback.FromUserID,
		chatID:      callback.Message.ChatID,
		nonce:       nonce,
		state:       stateAwaitingIPChangeConfirmation,
		expiresAt:   c.now().Add(c.ttl),
		panel:       Panel{ID: selected.PanelID, Name: selected.PanelName},
		ipTarget:    target,
		statusMsgID: callback.Message.ID,
	}
	c.putSession(session)
	if err := c.messenger.EditMessage(ctx, session.chatID, session.statusMsgID, renderIPChangeTarget(target), ipChangeKeyboard(nonce)); err != nil {
		if active := c.takeSession(session.userID, nonce, stateAwaitingIPChangeConfirmation); active != nil {
			active.clear()
		}
		return err
	}
	return nil
}

func (c *Controller) renderNodeIPStartFailure(ctx context.Context, callback *CallbackQuery, uuid string) error {
	return c.sendOrEdit(ctx, callback.Message.ChatID, callback.Message.ID, "Не удалось безопасно подготовить смену IP. Данные ноды могли измениться; обновите карточку и повторите.", Keyboard{Inline: [][]Button{{{Text: "🔄 Обновить карточку", CallbackData: "nodes:o:" + uuid}}}})
}

func formatNodeAlert(snapshot nodePanelSnapshot, node NodeSummary) string {
	return fmt.Sprintf("🚨 Критически низкий онлайн ноды\nПанель: %s\nНода: %s\nIP: %s\nОнлайн: %d\nКритический порог: менее %d\nОриентир панели: %d\n\nПроверьте блокировку IP и при необходимости замените его.", safeLine(node.PanelName, 80), safeLine(node.Name, 80), safeLine(node.Address, 80), node.Online, snapshot.Threshold, snapshot.Baseline)
}

func formatNodeRecovery(snapshot nodePanelSnapshot, node NodeSummary) string {
	return fmt.Sprintf("✅ Онлайн ноды восстановился\nПанель: %s\nНода: %s\nОнлайн: %d\nКритический порог: менее %d", safeLine(node.PanelName, 80), safeLine(node.Name, 80), node.Online, snapshot.Threshold)
}
