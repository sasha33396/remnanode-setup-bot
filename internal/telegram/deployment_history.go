package telegram

import (
	"fmt"
	"strings"
	"time"
)

func renderDeploymentsList(deployments []DeploymentSummary) string {
	var builder strings.Builder
	builder.WriteString("📜 Развёртывания\n")
	if len(deployments) == 0 {
		builder.WriteString("\nРазвёртываний пока нет.")
		return builder.String()
	}
	for _, item := range deployments {
		icon, status := deploymentStatusText(item.Status)
		completed, total := deploymentProgress(item.Status, item.CurrentStep)
		fmt.Fprintf(&builder, "\n%s [%s] %s — %s", icon, safeLine(item.PanelName, 40), safeLine(item.NodeName, 60), status)
		fmt.Fprintf(&builder, "\n   Этап: %s • %d/%d", deploymentStepText(item.CurrentStep), completed, total)
		if !item.UpdatedAt.IsZero() {
			fmt.Fprintf(&builder, "\n   Обновлено: %s", formatDeploymentTime(item.UpdatedAt))
		}
		if item.SafeErrorCode != "" {
			fmt.Fprintf(&builder, "\n   Код: %s", normalizeErrorCode(item.SafeErrorCode))
		}
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func deploymentListButton(item DeploymentSummary) string {
	icon, status := deploymentStatusText(item.Status)
	return safeLine(fmt.Sprintf("%s %s • %s", icon, item.NodeName, status), 54)
}

func renderDeploymentCard(details DeploymentDetails, notice string) string {
	var builder strings.Builder
	if notice != "" {
		builder.WriteString(truncateUTF8(notice, 1200))
		builder.WriteString("\n\n")
	}
	icon, status := deploymentStatusText(details.Status)
	completed, total := deploymentProgress(details.Status, details.CurrentStep)
	builder.WriteString("📜 Развёртывание\n")
	fmt.Fprintf(&builder, "%s Статус: %s\n", icon, status)
	fmt.Fprintf(&builder, "Панель: %s\n", safeLine(details.PanelName, 80))
	fmt.Fprintf(&builder, "Нода: %s\n", safeLine(details.NodeName, 80))
	if details.TargetIP.IsValid() {
		fmt.Fprintf(&builder, "IP сервера: %s\n", details.TargetIP)
	}
	fmt.Fprintf(&builder, "Текущий этап: %s\n", deploymentStepText(details.CurrentStep))
	fmt.Fprintf(&builder, "Прогресс: %d/%d", completed, total)
	if !details.CreatedAt.IsZero() {
		fmt.Fprintf(&builder, "\nСоздано: %s", formatDeploymentTime(details.CreatedAt))
	}
	if details.StartedAt != nil {
		fmt.Fprintf(&builder, "\nЗапущено: %s", formatDeploymentTime(*details.StartedAt))
	}
	if !details.UpdatedAt.IsZero() {
		fmt.Fprintf(&builder, "\nОбновлено: %s", formatDeploymentTime(details.UpdatedAt))
	}
	if details.CompletedAt != nil {
		fmt.Fprintf(&builder, "\nЗавершено: %s", formatDeploymentTime(*details.CompletedAt))
		if details.StartedAt != nil {
			fmt.Fprintf(&builder, "\nДлительность: %s", formatDeploymentDuration(details.CompletedAt.Sub(*details.StartedAt)))
		}
	}
	if details.SafeErrorCode != "" {
		fmt.Fprintf(&builder, "\n\n❌ Код ошибки: %s", normalizeErrorCode(details.SafeErrorCode))
	}
	if details.SafeError != "" {
		fmt.Fprintf(&builder, "\nОписание: %s", safeLine(details.SafeError, 500))
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func renderDeploymentLogs(details DeploymentDetails, entries []DeploymentLogEntry) string {
	var builder strings.Builder
	icon, status := deploymentStatusText(details.Status)
	builder.WriteString("📋 Подробный журнал развёртывания\n")
	fmt.Fprintf(&builder, "Нода: %s\nПанель: %s\n%s Состояние: %s", safeLine(details.NodeName, 80), safeLine(details.PanelName, 80), icon, status)
	if !details.UpdatedAt.IsZero() {
		fmt.Fprintf(&builder, "\nОбновлено: %s", formatDeploymentTime(details.UpdatedAt))
	}
	if len(entries) == 0 {
		builder.WriteString("\n\nЗаписей пока нет: этап ещё не начал выполнение.")
		return builder.String()
	}
	builder.WriteString("\n")
	for index, entry := range entries {
		entryIcon, entryStatus := deploymentLogStatus(entry.Status, entry.Code)
		fmt.Fprintf(&builder, "\n%s %02d. %s\n", entryIcon, index+1, deploymentLogStepText(entry.Step))
		fmt.Fprintf(&builder, "   Состояние: %s", entryStatus)
		if entry.Code != "" {
			code := normalizeWarningCode(entry.Code)
			if entryIcon == "❌" {
				code = normalizeErrorCode(entry.Code)
			}
			fmt.Fprintf(&builder, " • %s", code)
		}
		if summary := safeLine(deploymentSummaryText(entry.Summary), 150); summary != "" {
			fmt.Fprintf(&builder, "\n   Результат: %s", summary)
		}
		if entry.StartedAt != nil {
			fmt.Fprintf(&builder, "\n   Время: %s", formatDeploymentTime(*entry.StartedAt))
			if entry.CompletedAt != nil {
				fmt.Fprintf(&builder, " → %s • %s", formatDeploymentTime(*entry.CompletedAt), formatDeploymentDuration(entry.CompletedAt.Sub(*entry.StartedAt)))
			}
		} else if entry.CompletedAt != nil {
			fmt.Fprintf(&builder, "\n   Завершение: %s", formatDeploymentTime(*entry.CompletedAt))
		}
	}
	if details.SafeErrorCode != "" {
		fmt.Fprintf(&builder, "\n\n❌ Итоговая ошибка: %s", normalizeErrorCode(details.SafeErrorCode))
		if details.SafeError != "" {
			fmt.Fprintf(&builder, "\n%s", safeLine(details.SafeError, 400))
		}
	}
	return truncateUTF8(builder.String(), maxMessageBytes)
}

func deploymentLogsKeyboard(id string) Keyboard {
	return Keyboard{Inline: [][]Button{
		{{Text: "🔄 Обновить журнал", CallbackData: "dep:logs:" + id}},
		{{Text: "⬅️ К карточке", CallbackData: "dep:open:" + id}},
	}}
}

func deploymentStatusText(status string) (string, string) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "COMPLETED":
		return "✅", "завершено"
	case "FAILED":
		return "❌", "ошибка"
	case "DNS_FAILED":
		return "❌", "ошибка DNS"
	case "MANUAL_REVIEW":
		return "⚠️", "требует проверки"
	case "CANCELLED":
		return "⛔", "отменено"
	case "CREATED":
		return "🕓", "подготовлено"
	default:
		return "🔄", "выполняется"
	}
}

func deploymentLogStatus(status, code string) (string, string) {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "COMPLETED":
		return "✅", "выполнено без ошибок"
	case "FAILED", "DNS_FAILED":
		return "❌", "критическая ошибка"
	case "MANUAL_REVIEW":
		return "⚠️", "требует ручной проверки"
	case "SKIPPED":
		if strings.HasPrefix(normalizeWarningCode(code), "W-") && strings.TrimSpace(code) != "" {
			return "⚠️", "пропущено с предупреждением"
		}
		return "✅", "уже было настроено"
	case "CANCELLED":
		return "⛔", "отменено"
	case "PENDING":
		return "⬜", "ожидает запуска"
	default:
		return "🔄", "выполняется"
	}
}

func deploymentStepText(step string) string {
	step = strings.TrimSpace(step)
	if label := deploymentStepLabels[step]; label != "" {
		return label
	}
	if label := provisioningStepLabels[step]; label != "" {
		return "Настройка VPS → " + label
	}
	switch step {
	case "completed":
		return "Все этапы завершены"
	case "cancelled":
		return "Развёртывание отменено"
	case "created":
		return "Подготовка задания"
	case "workflow", "":
		return "Общий процесс"
	default:
		return safeLine(step, 80)
	}
}

func deploymentLogStepText(step string) string {
	if label := provisioningStepLabels[strings.TrimSpace(step)]; label != "" {
		return "Настройка VPS → " + label
	}
	return deploymentStepText(step)
}

func deploymentProgress(status, step string) (int, int) {
	if strings.EqualFold(status, "COMPLETED") {
		return 6, 6
	}
	if strings.EqualFold(status, "CREATED") {
		return 0, 6
	}
	if strings.EqualFold(status, "FAILED") && strings.TrimSpace(step) == progressStepPreflight {
		return 0, 6
	}
	switch strings.TrimSpace(step) {
	case progressStepPreflight:
		return 1, 6
	case "created":
		return 0, 6
	case progressStepCertificate:
		return 1, 6
	case progressStepProvisioning:
		return 2, 6
	case progressStepCreateNode:
		return 3, 6
	case progressStepWaitNode:
		return 4, 6
	case progressStepDNS:
		return 5, 6
	case "completed":
		return 6, 6
	default:
		return 0, 6
	}
}

func deploymentSummaryText(summary string) string {
	summary = safeLine(summary, 220)
	switch strings.ToLower(summary) {
	case "started":
		return "этап запущен"
	case "configured", "configured and validated":
		return "настроено и проверено"
	case "already completed", "already configured":
		return "уже было настроено"
	case "vps preflight passed":
		return "проверка VPS пройдена"
	case "certificate prepared":
		return "сертификат подготовлен"
	case "vps provisioning completed":
		return "настройка VPS завершена"
	case "remnawave node created":
		return "нода создана в Remnawave"
	case "remnawave node connected":
		return "нода подключилась к Remnawave"
	case "dns updated":
		return "DNS-балансировка обновлена"
	case "dns balancing is disabled for this panel":
		return "DNS-балансировка отключена для панели"
	case "inspection failed":
		return "не удалось проверить текущее состояние"
	case "apply failed":
		return "не удалось применить настройку"
	case "validation failed":
		return "настройка применена, но проверка не пройдена"
	default:
		return summary
	}
}

func formatDeploymentTime(value time.Time) string {
	return value.UTC().Format("02.01.2006 15:04:05 UTC")
}

func formatDeploymentDuration(value time.Duration) string {
	if value < 0 {
		value = 0
	}
	value = value.Round(time.Second)
	if value < time.Second {
		return "<1 сек"
	}
	hours := int(value / time.Hour)
	minutes := int(value%time.Hour) / int(time.Minute)
	seconds := int(value%time.Minute) / int(time.Second)
	switch {
	case hours > 0:
		return fmt.Sprintf("%d ч %d мин %d сек", hours, minutes, seconds)
	case minutes > 0:
		return fmt.Sprintf("%d мин %d сек", minutes, seconds)
	default:
		return fmt.Sprintf("%d сек", seconds)
	}
}
