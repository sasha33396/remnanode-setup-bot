// Package logging configures structured application logging.
package logging

import (
	"io"
	"log/slog"
)

// New returns a JSON logger suitable for container environments.
func New(output io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelInfo}))
}
