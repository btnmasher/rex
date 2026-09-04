package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNextBoundaryDelay(t *testing.T) {
	tests := []struct {
		name     string
		now      time.Time
		interval time.Duration
		want     time.Duration
	}{
		{
			name:     "minute boundary",
			now:      time.Date(2026, time.September, 3, 16, 1, 32, 500000000, time.UTC),
			interval: time.Minute,
			want:     27*time.Second + 500*time.Millisecond,
		},
		{
			name:     "quarter hour boundary",
			now:      time.Date(2026, time.September, 3, 16, 14, 59, 0, time.UTC),
			interval: 15 * time.Minute,
			want:     time.Second,
		},
		{
			name:     "exact boundary waits for next interval",
			now:      time.Date(2026, time.September, 3, 16, 15, 0, 0, time.UTC),
			interval: 15 * time.Minute,
			want:     15 * time.Minute,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nextBoundaryDelay(test.now, test.interval); got != test.want {
				t.Fatalf("next boundary delay = %s, want %s", got, test.want)
			}
		})
	}
}

func TestRunStopsBeforeFirstBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false

	err := Run(ctx, time.Minute, func(context.Context) {
		called = true
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("scheduler invoked job after cancellation")
	}
}
