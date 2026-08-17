package telegram

import (
	"errors"
	"strings"
	"testing"
)

func TestDeploymentProgressRendersLiveChecklistAndWarnings(t *testing.T) {
	view := newDeploymentProgressView([]OperatorNotice{{Code: "W-DOCKER-NOT-INSTALLED", Message: "Docker будет установлен автоматически"}})
	view.Update(Progress{Step: progressStepCertificate, Status: ProgressCompleted, SafeMessage: "Certificate prepared"})
	view.Update(Progress{Step: progressStepProvisioning, Status: ProgressRunning, SafeMessage: "Provisioning VPS"})
	view.Update(Progress{Step: "provisioning/system", Status: ProgressCompleted, SafeMessage: "configured and validated"})
	view.Update(Progress{Step: "provisioning/docker", Status: ProgressRunning, SafeMessage: "started"})

	text := view.Render()
	for _, expected := range []string{
		"🚀 Развёртывание ноды",
		"✅ Проверка VPS",
		"✅ Подготовка сертификата",
		"🔄 Настройка VPS — выполняется",
		"  ✅ Система и пакеты",
		"  🔄 Docker — выполняется",
		"⚠️ [W-DOCKER-NOT-INSTALLED] (Docker будет установлен автоматически)",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("progress is missing %q:\n%s", expected, text)
		}
	}
}

func TestDeploymentProgressShowsSafeCodedFailureAndRecoveryAction(t *testing.T) {
	view := newDeploymentProgressView(nil)
	view.Update(Progress{Step: progressStepProvisioning, Status: ProgressRunning})
	view.Fail(fakeOperatorError{code: "PROVISIONING_FAILED", message: "Настройка VPS завершилась ошибкой"})

	text := view.Render()
	for _, expected := range []string{
		"❌ Развёртывание остановлено",
		"❌ Настройка VPS — [E-PROVISIONING-FAILED] Настройка VPS завершилась ошибкой",
		"«Безопасные логи» или «Повторить шаг»",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("failure progress is missing %q:\n%s", expected, text)
		}
	}

	view = newDeploymentProgressView(nil)
	view.Fail(errors.New("password=do-not-expose"))
	if strings.Contains(view.Render(), "do-not-expose") {
		t.Fatal("arbitrary error text was exposed")
	}
}

type fakeOperatorError struct {
	code    string
	message string
}

func (e fakeOperatorError) Error() string           { return e.message }
func (e fakeOperatorError) OperatorCode() string    { return e.code }
func (e fakeOperatorError) OperatorMessage() string { return e.message }
