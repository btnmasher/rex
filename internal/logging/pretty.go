package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/lipgloss"
)

const prettyFieldIndent = 2

type prettyHandler struct {
	mu     *sync.Mutex
	out    io.Writer
	level  slog.Level
	attrs  []prettyAttr
	groups []string
}

// NewPrettyHandler creates a terminal-oriented slog handler with compact
// colored levels and deterministic structured attributes.
func NewPrettyHandler(writer io.Writer, options *slog.HandlerOptions) slog.Handler {
	if writer == nil {
		writer = io.Discard
	}
	level := slog.LevelInfo
	if options != nil && options.Level != nil {
		level = options.Level.Level()
	}
	return &prettyHandler{
		mu:    &sync.Mutex{},
		out:   writer,
		level: level,
	}
}

func (h *prettyHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

//nolint:gocritic // slog.Handler requires slog.Record by value.
func (h *prettyHandler) Handle(_ context.Context, record slog.Record) error {
	fields := make([]prettyField, 0, len(h.attrs)+record.NumAttrs())
	for _, attr := range h.attrs {
		appendPrettyField(&fields, attr.prefix, attr.attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		appendPrettyField(&fields, strings.Join(h.groups, "."), attr)
		return true
	})
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].key < fields[j].key
	})
	line := prettyHeader(record.Time, record.Level, record.Message)
	if len(fields) > 0 {
		line += "\n" + renderFields(fields)
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.out, line)
	return err
}

func (h *prettyHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := h.clone()
	prefix := strings.Join(h.groups, ".")
	for _, attr := range attrs {
		clone.attrs = append(clone.attrs, prettyAttr{prefix: prefix, attr: attr})
	}
	return clone
}

func (h *prettyHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	clone := h.clone()
	clone.groups = append(clone.groups, name)
	return clone
}

func (h *prettyHandler) clone() *prettyHandler {
	clone := *h
	clone.attrs = append([]prettyAttr(nil), h.attrs...)
	clone.groups = append([]string(nil), h.groups...)
	return &clone
}

type prettyField struct {
	key   string
	value string
}

type prettyAttr struct {
	prefix string
	attr   slog.Attr
}

func prettyHeader(timestamp time.Time, level slog.Level, message string) string {
	timeLabel := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(timestamp.Format("15:04:05.000"))
	levelLabel, levelStyle := levelBadge(level)
	messageLabel := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Render(message)
	return lipgloss.JoinHorizontal(lipgloss.Center, timeLabel, " ", levelStyle.Render(levelLabel), " ", messageLabel)
}

func levelBadge(level slog.Level) (string, lipgloss.Style) {
	base := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	switch {
	case level <= slog.LevelDebug:
		return "DEBUG", base.Foreground(lipgloss.Color("236")).Background(lipgloss.Color("239"))
	case level <= slog.LevelInfo:
		return "INFO", base.Foreground(lipgloss.Color("230")).Background(lipgloss.Color("31"))
	case level <= slog.LevelWarn:
		return "WARN", base.Foreground(lipgloss.Color("234")).Background(lipgloss.Color("214"))
	default:
		return "ERROR", base.Foreground(lipgloss.Color("231")).Background(lipgloss.Color("160"))
	}
}

func appendPrettyField(fields *[]prettyField, prefix string, attr slog.Attr) {
	if attr.Key == "" {
		return
	}
	value := attr.Value.Resolve()
	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if value.Kind() == slog.KindGroup {
		for _, child := range value.Group() {
			appendPrettyField(fields, key, child)
		}
		return
	}
	*fields = append(*fields, prettyField{key: key, value: fmt.Sprint(value.Any())})
}

func renderFields(fields []prettyField) string {
	parts := make([]string, 0, len(fields))
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	valueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	separatorStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	for _, field := range fields {
		parts = append(parts, keyStyle.Render(field.key)+separatorStyle.Render("=")+valueStyle.Render(field.value))
	}
	return lipgloss.NewStyle().MarginLeft(prettyFieldIndent).Render(strings.Join(parts, " "))
}
