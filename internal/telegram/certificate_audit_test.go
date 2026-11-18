package telegram

import (
	"context"
	"strings"
	"testing"
	"time"
)

type fakeCertificateAuditApplication struct {
	*fakeApplication
	result CertificateAuditResult
	err    error
}

func (f *fakeCertificateAuditApplication) AuditCertificates(context.Context) (CertificateAuditResult, error) {
	return f.result, f.err
}

func TestCertificateAuditMenuRunsInBackgroundAndRendersResult(t *testing.T) {
	application := &fakeCertificateAuditApplication{
		fakeApplication: &fakeApplication{},
		result:          CertificateAuditResult{Panels: 4, Hosts: 8, Checked: 12, Healthy: 10, Renewed: 1, Repaired: 2},
	}
	messenger := &fakeMessenger{}
	controller := testController(t, application, messenger, func() time.Time { return time.Unix(100, 0) })
	handleMessage(t, controller, 1, MenuCertificates)
	controller.Wait()
	edits := messenger.editsSnapshot()
	if len(edits) == 0 || !strings.Contains(edits[len(edits)-1].text, "Host: 8") || !strings.Contains(edits[len(edits)-1].text, "Перевыпущено сертификатов: 1") {
		t.Fatalf("certificate audit result = %#v", edits)
	}
}
