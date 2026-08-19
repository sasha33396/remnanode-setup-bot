package telegram

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestDeploymentHistoryRendersLocalizedDetailedListAndCard(t *testing.T) {
	created := time.Date(2026, 8, 17, 18, 20, 0, 0, time.UTC)
	started := created.Add(time.Minute)
	updated := started.Add(4 * time.Minute)
	item := DeploymentSummary{
		PanelName:   "Hit",
		ID:          "1c60754a-f9bb-41fa-95bb-39f11375bbaa",
		NodeName:    "un2-cherry-new-1",
		Status:      "PROVISIONING",
		CurrentStep: progressStepProvisioning,
		TargetIP:    netip.MustParseAddr("8.8.8.8"),
		CreatedAt:   created,
		StartedAt:   &started,
		UpdatedAt:   updated,
	}
	list := renderDeploymentsList([]DeploymentSummary{item})
	for _, expected := range []string{"🔄 [Hit] un2-cherry-new-1 — выполняется", "Этап: Настройка VPS • 2/6", "17.08.2026 18:25:00 UTC"} {
		if !strings.Contains(list, expected) {
			t.Fatalf("deployment list is missing %q:\n%s", expected, list)
		}
	}
	if strings.Contains(list, "PROVISIONING") {
		t.Fatalf("raw status leaked into localized list: %s", list)
	}

	card := renderDeploymentCard(DeploymentDetails{DeploymentSummary: item}, "")
	for _, expected := range []string{"🔄 Статус: выполняется", "IP сервера: 8.8.8.8", "Текущий этап: Настройка VPS", "Прогресс: 2/6", "Запущено:"} {
		if !strings.Contains(card, expected) {
			t.Fatalf("deployment card is missing %q:\n%s", expected, card)
		}
	}
}

func TestDeploymentHistoryRendersStructuredLogsCodesAndDurations(t *testing.T) {
	started := time.Date(2026, 8, 17, 18, 20, 0, 0, time.UTC)
	completed := started.Add(7*time.Second + 400*time.Millisecond)
	details := DeploymentDetails{DeploymentSummary: DeploymentSummary{PanelName: "Hit", NodeName: "node", Status: "FAILED", CurrentStep: progressStepProvisioning, SafeErrorCode: "PROVISIONING_FAILED", SafeError: "VPS provisioning failed", UpdatedAt: completed}}
	entries := []DeploymentLogEntry{
		{Step: progressStepPreflight, Status: "COMPLETED", Summary: "VPS preflight passed", StartedAt: &started, CompletedAt: &completed},
		{Step: "logrotate", Status: "FAILED", Summary: "validation failed", Code: "E-PROVISIONING-LOGROTATE", StartedAt: &started, CompletedAt: &completed},
	}
	logs := renderDeploymentLogs(details, entries)
	for _, expected := range []string{
		"📋 Подробный журнал развёртывания",
		"✅ 01. Проверка VPS",
		"Состояние: выполнено без ошибок",
		"Результат: проверка VPS пройдена",
		"❌ 02. Настройка VPS → Ротация логов",
		"E-PROVISIONING-LOGROTATE",
		"настройка применена, но проверка не пройдена",
		"7 сек",
		"Итоговая ошибка: E-PROVISIONING-FAILED",
	} {
		if !strings.Contains(logs, expected) {
			t.Fatalf("deployment logs are missing %q:\n%s", expected, logs)
		}
	}
}
