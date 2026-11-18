package telegram

import (
	"context"
	"fmt"
	"strings"
)

func (c *Controller) startCertificateAudit(ctx context.Context, message *Message) error {
	application, ok := c.app.(CertificateAuditApplication)
	if !ok {
		_, err := c.messenger.SendMessage(ctx, message.ChatID, "Проверка сертификатов недоступна.", mainKeyboard())
		return err
	}
	status, err := c.messenger.SendMessage(ctx, message.ChatID, "⏳ Проверяю сертификаты Host и фактически работающие сертификаты нод…", Keyboard{})
	if err != nil {
		return err
	}
	c.workers.Add(1)
	go func(chatID int64, messageID int) {
		defer c.workers.Done()
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deploymentActionTimeout)
		defer cancel()
		result, auditErr := application.AuditCertificates(runCtx)
		text := renderCertificateAudit(result, auditErr)
		_ = c.messenger.EditMessage(runCtx, chatID, messageID, text, mainKeyboard())
	}(message.ChatID, status.ID)
	return nil
}

func renderCertificateAudit(result CertificateAuditResult, err error) string {
	status := "✅ Проверка сертификатов завершена"
	if err != nil || len(result.Failures) > 0 {
		status = "⚠️ Проверка сертификатов завершена с ошибками"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\n\nПанелей: %d\nHost: %d\nНод проверено: %d\nИсправных: %d\nПеревыпущено сертификатов: %d\nИсправлено на нодах: %d\nНедоступных нод: %d", status, result.Panels, result.Hosts, result.Checked, result.Healthy, result.Renewed, result.Repaired, result.Deferred)
	if len(result.Failures) > 0 {
		builder.WriteString("\n\nОшибки:")
		limit := len(result.Failures)
		if limit > 15 {
			limit = 15
		}
		for _, failure := range result.Failures[:limit] {
			label := failure.PanelName
			if failure.SNI != "" {
				label += " / " + failure.SNI
			}
			fmt.Fprintf(&builder, "\n• %s — %s", safeLine(label, 100), certificateAuditReason(failure.Reason))
		}
		if len(result.Failures) > limit {
			fmt.Fprintf(&builder, "\n…ещё %d", len(result.Failures)-limit)
		}
	}
	return builder.String()
}

func certificateAuditReason(reason string) string {
	switch reason {
	case "hosts_unavailable":
		return "не удалось получить Host из панели"
	case "audit_unavailable":
		return "проверка недоступна для панели"
	case "nodes_unreachable":
		return "часть нод недоступна, повтор будет выполнен планировщиком"
	case "audit_failed":
		return "проверка не завершена"
	default:
		return safeLine(reason, 180)
	}
}
