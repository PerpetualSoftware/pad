// Package logging provides structured logging using log/slog.
//
// Usage:
//
//	logging.Setup("info", "json")   // call once at startup
//	slog.Info("something happened", "key", value)
//
// All application code should use the slog package directly after Setup has
// been called — it configures the default slog logger.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// Setup configures the default slog logger.
//
//   - level: "debug", "info", "warn", "error" (default "info")
//   - format: "json" or "text" (default "text")
//
// After calling Setup, use slog.Info / slog.Error / etc. everywhere.
func Setup(level, format string) {
	SetupWriter(os.Stderr, level, format)
}

// SetupWriter is like Setup but writes to w instead of stderr (useful for tests).
func SetupWriter(w io.Writer, level, format string) {
	lvl := parseLevel(level)

	opts := &slog.HandlerOptions{
		Level: lvl,
	}

	var handler slog.Handler
	switch strings.ToLower(format) {
	case "json":
		handler = slog.NewJSONHandler(w, opts)
	default:
		handler = slog.NewTextHandler(w, opts)
	}

	slog.SetDefault(slog.New(handler))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
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
