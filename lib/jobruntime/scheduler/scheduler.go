// Package scheduler provides wall-clock-aligned periodic scheduling.
package scheduler

import (
	"context"
	"errors"
	"time"
)

// Run invokes job at each future wall-clock boundary for interval until ctx is
// canceled. A running job suppresses boundaries that occur during its run.
func Run(ctx context.Context, interval time.Duration, job func(context.Context)) error {
	if ctx == nil {
		return errors.New("scheduler context is required")
	}
	if interval <= 0 {
		return errors.New("scheduler interval must be positive")
	}
	if job == nil {
		return errors.New("scheduler job is required")
	}

	for {
		timer := time.NewTimer(nextBoundaryDelay(time.Now(), interval))
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return ctx.Err()
		case <-timer.C:
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		job(ctx)
	}
}

func nextBoundaryDelay(now time.Time, interval time.Duration) time.Duration {
	next := now.Truncate(interval).Add(interval)
	return next.Sub(now)
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}
