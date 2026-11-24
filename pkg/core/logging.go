package core

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// InitLogger configure le logger structuré global selon ASTRAL_LOG
// (debug, info, warn, error). Par défaut : info.
func InitLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ASTRAL_LOG"))) {
	case "debug", "trace":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
	})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
