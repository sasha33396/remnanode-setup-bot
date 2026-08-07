package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLoggerRedactsSecretKeysAndValues(t *testing.T) {
	var output bytes.Buffer
	logger := NewWithSecrets(&output, "literal-sensitive-value")
	logger.Error("request failed", "token", "token-value", "error", errors.New("upstream included literal-sensitive-value"), "safe", "visible")
	text := output.String()
	for _, secret := range []string{"token-value", "literal-sensitive-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "visible") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("unexpected redacted log: %s", text)
	}
}
