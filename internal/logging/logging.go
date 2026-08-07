// Package logging configures structured application logging.
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a JSON logger suitable for container environments.
func New(output io.Writer) *slog.Logger {
	return NewWithSecrets(output)
}

// NewWithSecrets redacts secret-keyed attributes and configured secret values
// before structured records reach the output.
func NewWithSecrets(output io.Writer, secrets ...string) *slog.Logger {
	filtered := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if strings.TrimSpace(secret) != "" {
			filtered = append(filtered, secret)
		}
	}
	handler := slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo, ReplaceAttr: func(_ []string, attribute slog.Attr) slog.Attr {
		key := strings.ToLower(attribute.Key)
		for _, marker := range []string{"token", "password", "private_key", "secret", "database_url", "authorization"} {
			if strings.Contains(key, marker) {
				attribute.Value = slog.StringValue("[REDACTED]")
				return attribute
			}
		}
		var value string
		switch attribute.Value.Kind() {
		case slog.KindString:
			value = attribute.Value.String()
		case slog.KindAny:
			if err, ok := attribute.Value.Any().(error); ok {
				value = err.Error()
			}
		}
		if value != "" {
			for _, secret := range filtered {
				value = strings.ReplaceAll(value, secret, "[REDACTED]")
			}
			attribute.Value = slog.StringValue(value)
		}
		return attribute
	}})
	return slog.New(handler)
}
