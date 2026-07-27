// Package logging builds the shared structured logger.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger at the given level. Unrecognised levels fall back
// to info rather than failing: a typo in LOG_LEVEL should not stop the service.
func New(level string) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
