package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSafePreflightFailurePreservesOnlyTrustedPhase(t *testing.T) {
	code, message := safePreflightFailure(preflightFailure("SSH_CONNECTION_FAILED", "safe SSH guidance"))
	if code != "SSH_CONNECTION_FAILED" || message != "safe SSH guidance" {
		t.Fatalf("safePreflightFailure() = %q, %q", code, message)
	}

	code, message = safePreflightFailure(errors.New("password=protected-value"))
	if code != "PREFLIGHT_FAILED" || strings.Contains(message, "protected-value") {
		t.Fatalf("untrusted failure leaked details: %q, %q", code, message)
	}
}

func TestSafeInitialSSHFailureClassifiesAuthenticationWithoutLeakingDetails(t *testing.T) {
	err := safeInitialSSHFailure(context.Background(), errors.New("ssh: unable to authenticate, attempted methods [none password], password=protected-value"))
	code, message := safePreflightFailure(err)
	if code != "SSH_AUTHENTICATION_FAILED" || strings.Contains(message, "protected-value") {
		t.Fatalf("safe SSH failure = %q, %q", code, message)
	}
}

func TestSelfSNINginxUsesHostForCertificateAndNotRealitySNI(t *testing.T) {
	config := selfSNINginxConfig(ProvisionVPSInput{
		SNIDomain:  "entry.example.com",
		RealitySNI: "storage.accoil.ru",
	}, SSHProvisionerConfig{
		ACMEEmail:       "ops@example.com",
		CloudflareToken: []byte("token"),
		CommandTimeout:  time.Minute,
	})

	if config.CertificateDomain != "entry.example.com" {
		t.Fatalf("certificate domain = %q, want Host FQDN", config.CertificateDomain)
	}
	if config.CertificateDomain == "storage.accoil.ru" {
		t.Fatal("Reality serverName was incorrectly used as the Cloudflare certificate domain")
	}
}
