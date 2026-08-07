package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRegistryExposesProductionMetrics(t *testing.T) {
	registry := New()
	registry.DeploymentCreated()
	registry.DeploymentFailed()
	registry.ActiveDeployment(1)
	registry.DeploymentDuration(2 * time.Second)
	registry.ProvisioningStepDuration("docker", time.Second)
	registry.RemnawaveAPIError()
	registry.DNSAPIError()
	registry.SetCertificateExpiry("edge.example.com", 10*24*time.Hour)
	registry.CertificateRenewalFailed("edge.example.com")
	recorder := httptest.NewRecorder()
	registry.ServeHTTP(recorder, httptest.NewRequest("GET", "/metrics", nil))
	for _, name := range []string{"deployments_total", "deployments_failed_total", "active_deployments", "deployment_duration_seconds", "provisioning_step_duration_seconds", "remnawave_api_errors_total", "dns_api_errors_total", "certificate_expiry_days", "certificate_renewal_failures_total"} {
		if !strings.Contains(recorder.Body.String(), name) {
			t.Errorf("metrics output does not contain %s", name)
		}
	}
}
