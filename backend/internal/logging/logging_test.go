package logging

import (
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  slog.Level
	}{
		{"debug", "debug", slog.LevelDebug},
		{"info", "info", slog.LevelInfo},
		{"warn", "warn", slog.LevelWarn},
		{"warning alias", "warning", slog.LevelWarn},
		{"error", "error", slog.LevelError},
		{"uppercase is accepted", "ERROR", slog.LevelError},
		{"surrounding whitespace is trimmed", "  debug  ", slog.LevelDebug},
		// A typo in LOG_LEVEL should not stop the service from starting.
		{"unknown falls back to info", "verbose", slog.LevelInfo},
		{"empty falls back to info", "", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseLevel(tt.input); got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNewRespectsLevel(t *testing.T) {
	logger := New("error")

	if logger.Enabled(t.Context(), slog.LevelInfo) {
		t.Error("logger built at error level should not emit info records")
	}
	if !logger.Enabled(t.Context(), slog.LevelError) {
		t.Error("logger built at error level should emit error records")
	}
}
