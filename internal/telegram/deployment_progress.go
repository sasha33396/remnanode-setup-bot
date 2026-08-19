package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	progressStepPreflight    = "preflight"
	progressStepCertificate  = "prepare_certificate"
	progressStepProvisioning = "provisioning"
	progressStepCreateNode   = "create_remnawave_node"
	progressStepWaitNode     = "wait_remnawave"
	progressStepDNS          = "add_dns"
)

var deploymentStepOrder = []string{
	progressStepPreflight,
	progressStepCertificate,
	progressStepProvisioning,
	progressStepCreateNode,
	progressStepWaitNode,
	progressStepDNS,
}

var deploymentStepLabels = map[string]string{
	progressStepPreflight:    "Проверка VPS",
	progressStepCertificate:  "Подготовка сертификата",
	progressStepProvisioning: "Настройка VPS",
	progressStepCreateNode:   "Создание ноды в Remnawave",
	progressStepWaitNode:     "Подключение ноды",
	progressStepDNS:          "DNS-балансировка",
}

var provisioningStepOrder = []string{
	"system", "docker", "sysctl", "limits", "firewall", "fail2ban",
	"remnanode", "node_exporter", "speedtest_exporter", "logrotate", "xray_sni", "healthcheck",
}

var provisioningStepLabels = map[string]string{
	"system":             "Система и пакеты",
	"docker":             "Docker",
	"sysctl":             "Параметры ядра",
	"limits":             "Системные лимиты",
	"firewall":           "Файрвол",
	"fail2ban":           "Fail2Ban",
	"remnanode":          "Remnanode",
	"node_exporter":      "Node Exporter",
	"speedtest_exporter": "Speedtest Exporter",
	"logrotate":          "Ротация логов",
	"xray_sni":           "Xray SNI",
	"healthcheck":        "Проверка сервисов",
}

type deploymentProgressEntry struct {
	status  ProgressStatus
	code    string
	message string
}

type deploymentProgressView struct {
	steps      map[string]deploymentProgressEntry
	provision  map[string]deploymentProgressEntry
	warnings   []OperatorNotice
	failed     bool
	completed  bool
	activeStep string
}

func newDeploymentProgressView(warnings []OperatorNotice) *deploymentProgressView {
	view := &deploymentProgressView{
		steps:     make(map[string]deploymentProgressEntry, len(deploymentStepOrder)),
		provision: make(map[string]deploymentProgressEntry, len(provisioningStepOrder)),
		warnings:  append([]OperatorNotice(nil), warnings...),
	}
	view.steps[progressStepPreflight] = deploymentProgressEntry{status: ProgressCompleted, message: "VPS preflight passed"}
	view.steps[progressStepCertificate] = deploymentProgressEntry{status: ProgressRunning, message: "Preparing certificate"}
	view.activeStep = progressStepCertificate
	return view
}

func (v *deploymentProgressView) Update(progress Progress) {
	step := strings.TrimSpace(progress.Step)
	if step == "" || step == "starting" {
		return
	}
	status := progress.Status
	if status == "" {
		status = ProgressRunning
		if progress.Total > 0 && progress.Completed >= progress.Total {
			status = ProgressCompleted
		}
	}
	entry := deploymentProgressEntry{status: status, code: normalizeOperatorCode(progress.Code, status), message: safeLine(progress.SafeMessage, 180)}
	if parent, child, found := strings.Cut(step, "/"); found && parent == progressStepProvisioning {
		v.completeBefore(parent)
		v.provision[child] = entry
		v.steps[progressStepProvisioning] = deploymentProgressEntry{status: ProgressRunning, message: "Provisioning VPS"}
		v.activeStep = progressStepProvisioning
		if status == ProgressFailed {
			v.steps[progressStepProvisioning] = entry
			v.failed = true
		}
		return
	}
	if _, known := deploymentStepLabels[step]; !known {
		return
	}
	v.completeBefore(step)
	v.steps[step] = entry
	if status == ProgressRunning {
		v.activeStep = step
	}
	if status == ProgressFailed {
		v.failed = true
	}
}

func (v *deploymentProgressView) completeBefore(step string) {
	for _, candidate := range deploymentStepOrder {
		if candidate == step {
			return
		}
		entry := v.steps[candidate]
		if entry.status == "" || entry.status == ProgressRunning {
			entry.status = ProgressCompleted
			entry.code = ""
			v.steps[candidate] = entry
		}
	}
}

type operatorSafeError interface {
	OperatorCode() string
	OperatorMessage() string
}

func (v *deploymentProgressView) Fail(err error) {
	code, message := "E-DEPLOYMENT-FAILED", "Развёртывание остановлено"
	var safe operatorSafeError
	if errors.As(err, &safe) {
		code = normalizeErrorCode(safe.OperatorCode())
		message = safeLine(safe.OperatorMessage(), 220)
	} else if errors.Is(err, context.Canceled) {
		code, message = "E-DEPLOYMENT-CANCELLED", "Развёртывание отменено"
	}
	step := deploymentStepForError(code, v.activeStep)
	v.completeBefore(step)
	v.steps[step] = deploymentProgressEntry{status: ProgressFailed, code: code, message: message}
	v.activeStep = step
	v.failed = true
}

func (v *deploymentProgressView) Complete() {
	for _, step := range deploymentStepOrder {
		entry := v.steps[step]
		if entry.status == "" || entry.status == ProgressRunning {
			entry.status = ProgressCompleted
			v.steps[step] = entry
		}
	}
	v.completed = true
}

func (v *deploymentProgressView) Render() string {
	var builder strings.Builder
	switch {
	case v.failed:
		builder.WriteString("❌ Развёртывание остановлено\n")
	case v.completed:
		builder.WriteString("✅ Нода успешно развёрнута\n")
	default:
		builder.WriteString("🚀 Развёртывание ноды\n")
	}
	fmt.Fprintf(&builder, "Прогресс: %d/%d\n\n", v.completedCount(), len(deploymentStepOrder))
	for _, step := range deploymentStepOrder {
		entry := v.steps[step]
		fmt.Fprintf(&builder, "%s %s%s\n", progressIcon(entry), deploymentStepLabels[step], progressSuffix(entry))
		if step == progressStepProvisioning && len(v.provision) > 0 {
			for _, child := range provisioningStepOrder {
				childEntry := v.provision[child]
				fmt.Fprintf(&builder, "  %s %s%s\n", progressIcon(childEntry), provisioningStepLabels[child], progressSuffix(childEntry))
			}
		}
	}
	warnings := v.unattachedWarnings()
	if len(warnings) > 0 {
		builder.WriteString("\nОбратите внимание:\n")
		for _, warning := range warnings {
			fmt.Fprintf(&builder, "⚠️ [%s] (%s)\n", normalizeWarningCode(warning.Code), safeLine(warning.Message, 180))
		}
	}
	if v.failed {
		builder.WriteString("\nЧто делать: откройте «📜 Развёртывания», выберите эту ноду и используйте «Безопасные логи» или «Повторить шаг».")
	}
	return truncateUTF8(strings.TrimSpace(builder.String()), maxMessageBytes)
}

func (v *deploymentProgressView) completedCount() int {
	count := 0
	for _, step := range deploymentStepOrder {
		switch v.steps[step].status {
		case ProgressCompleted, ProgressWarning, ProgressSkipped:
			count++
		}
	}
	return count
}

func (v *deploymentProgressView) unattachedWarnings() []OperatorNotice {
	attached := make(map[string]struct{})
	for _, entry := range v.steps {
		if entry.code != "" {
			attached[entry.code] = struct{}{}
		}
	}
	result := make([]OperatorNotice, 0, len(v.warnings))
	for _, warning := range v.warnings {
		code := normalizeWarningCode(warning.Code)
		if _, exists := attached[code]; exists {
			continue
		}
		result = append(result, OperatorNotice{Code: code, Message: warning.Message})
	}
	return result
}

func progressIcon(entry deploymentProgressEntry) string {
	switch entry.status {
	case ProgressRunning:
		return "🔄"
	case ProgressCompleted:
		return "✅"
	case ProgressWarning:
		return "⚠️"
	case ProgressSkipped:
		if strings.HasPrefix(entry.code, "W-") {
			return "⚠️"
		}
		return "✅"
	case ProgressFailed:
		return "❌"
	default:
		return "⬜"
	}
}

func progressSuffix(entry deploymentProgressEntry) string {
	switch entry.status {
	case ProgressRunning:
		return " — выполняется"
	case ProgressSkipped:
		if strings.HasPrefix(entry.code, "W-") {
			return fmt.Sprintf(" — [%s] (%s)", entry.code, fallbackText(entry.message, "пропущено с предупреждением"))
		}
		return " — уже было настроено"
	case ProgressWarning:
		return fmt.Sprintf(" — [%s] (%s)", fallbackText(entry.code, "W-GENERAL"), fallbackText(entry.message, "требует внимания"))
	case ProgressFailed:
		return fmt.Sprintf(" — [%s] %s", fallbackText(entry.code, "E-DEPLOYMENT-FAILED"), fallbackText(entry.message, "операция завершилась ошибкой"))
	default:
		return ""
	}
}

func normalizeOperatorCode(code string, status ProgressStatus) string {
	if status == ProgressFailed {
		return normalizeErrorCode(code)
	}
	if status == ProgressWarning || status == ProgressSkipped {
		if strings.TrimSpace(code) != "" {
			return normalizeWarningCode(code)
		}
	}
	return ""
}

func normalizeWarningCode(code string) string {
	code = normalizeCode(code)
	if strings.HasPrefix(code, "W-") {
		return code
	}
	if code == "" {
		return "W-GENERAL"
	}
	return "W-" + code
}

func normalizeErrorCode(code string) string {
	code = normalizeCode(code)
	if strings.HasPrefix(code, "E-") {
		return code
	}
	if code == "" {
		return "E-DEPLOYMENT-FAILED"
	}
	return "E-" + code
}

func normalizeCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	code = strings.NewReplacer("_", "-", " ", "-").Replace(code)
	return strings.Trim(code, "-")
}

func deploymentStepForError(code, fallback string) string {
	switch {
	case strings.Contains(code, "PREFLIGHT"):
		return progressStepPreflight
	case strings.Contains(code, "CERTIFICATE") || strings.Contains(code, "ACME"):
		return progressStepCertificate
	case strings.Contains(code, "PROVISION") || strings.Contains(code, "KEYGEN"):
		return progressStepProvisioning
	case strings.Contains(code, "NODE-CREATE") || strings.Contains(code, "DUPLICATE") || strings.Contains(code, "NODE-UUID"):
		return progressStepCreateNode
	case strings.Contains(code, "NODE-CONNECTION") || strings.Contains(code, "NODE-WAIT") || strings.Contains(code, "NODE-NOT-CONNECTED"):
		return progressStepWaitNode
	case strings.Contains(code, "DNS") || strings.Contains(code, "NODE-NOT-HEALTHY"):
		return progressStepDNS
	case deploymentStepLabels[fallback] != "":
		return fallback
	default:
		return progressStepProvisioning
	}
}

func fallbackText(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
