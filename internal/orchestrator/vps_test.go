package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
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
