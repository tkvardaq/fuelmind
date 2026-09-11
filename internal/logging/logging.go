// Package logging is a thin wrapper around log/slog. We keep it tiny on
// purpose: slog is the standard, and we don't want to invent a logger.
package logging

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a *slog.Logger writing structured text records to stderr at
// the given level. Phase 8 (the installer) will swap stderr for a rotating
// file under the data directory; for now, stderr is fine for dev and
// matches what Windows services see in the event log when redirected.
func New(level string) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: parseLevel(level),
	}))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
