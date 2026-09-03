// Package logging provides Rex's structured logging composition and local
// developer presentation.
package logging

import (
	"log/slog"
	"os"
	"strings"

	"golang.org/x/term"
)

// New creates a logger with the requested minimum level. Pretty output is
// enabled only when requested and stderr is an interactive terminal; all
// other output remains newline-delimited JSON for machine consumption.
func New(level string, pretty bool) *slog.Logger {
	minimum := parseLevel(level)
	if pretty && term.IsTerminal(int(os.Stderr.Fd())) {
		return slog.New(NewPrettyHandler(os.Stderr, &slog.HandlerOptions{Level: minimum}))
	}

	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: minimum}))
}

func parseLevel(value string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
