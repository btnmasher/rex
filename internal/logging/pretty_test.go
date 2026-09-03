package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestPrettyHandlerRendersStructuredAttributesAndGroups(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(NewPrettyHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logger.With("job", "token-refresh").WithGroup("esi").DebugContext(
		context.Background(),
		"request completed",
		slog.Int("status", 200),
	)

	for _, want := range []string{"DEBUG", "request completed", "job=token-refresh", "esi.status=200"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("pretty output missing %q: %q", want, output.String())
		}
	}
}
