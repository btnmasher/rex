package jobs

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

func TestForEachItemStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := 0
	err := ForEachItem(ctx, []int{1, 2, 3}, func(_ int) error {
		called++
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if called != 0 {
		t.Fatalf("expected no item processing, called=%d", called)
	}
}

func TestCollectStepResultsCollectsAndPropagates(t *testing.T) {
	items := []testItem{
		{ItemMeta{ItemID: "1"}},
		{ItemMeta{ItemID: "2"}},
	}
	results, err := CollectStepResults(context.Background(), items, func(item testItem) (StepResult[testItem], error) {
		return Succeeded(item), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	wantErr := errors.New("boom")
	_, err = CollectStepResults(context.Background(), items, func(testItem) (StepResult[testItem], error) {
		return StepResult[testItem]{}, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
}

func TestIterateStepResults(t *testing.T) {
	items := []testItem{
		{ItemID: "1"},
		{ItemID: "2"},
	}
	results, err := IterateStepResults(context.Background(), items, func(_ context.Context, item testItem) StepResult[testItem] {
		return Succeeded(item)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

type iterateProgressRuntime struct {
	progressCalls int
	lastDone      int
	lastTotal     int
}

func (iterateProgressRuntime) RunID() string         { return "" }
func (iterateProgressRuntime) JobID() string         { return "" }
func (iterateProgressRuntime) JobKind() string       { return "" }
func (iterateProgressRuntime) StepName() string      { return "" }
func (iterateProgressRuntime) RetryGroupID() *string { return nil }
func (iterateProgressRuntime) Logger() *slog.Logger  { return slog.Default() }
func (iterateProgressRuntime) Debug(context.Context, string, ...slog.Attr) error {
	return nil
}
func (iterateProgressRuntime) Debugf(context.Context, string, string, ...any) error {
	return nil
}
func (iterateProgressRuntime) Info(context.Context, string, ...slog.Attr) error {
	return nil
}
func (iterateProgressRuntime) Infof(context.Context, string, string, ...any) error {
	return nil
}
func (iterateProgressRuntime) Warn(context.Context, string, ...slog.Attr) error {
	return nil
}
func (iterateProgressRuntime) Warnf(context.Context, string, string, ...any) error {
	return nil
}
func (iterateProgressRuntime) Error(context.Context, string, ...slog.Attr) error {
	return nil
}
func (iterateProgressRuntime) Errorf(context.Context, string, string, ...any) error {
	return nil
}
func (r *iterateProgressRuntime) Progress(_ context.Context, done, total int, _ ...slog.Attr) error {
	r.progressCalls++
	r.lastDone = done
	r.lastTotal = total
	return nil
}

func TestIterateStepResultsWithProgress(t *testing.T) {
	items := []testItem{
		{ItemID: "1"},
		{ItemID: "2"},
	}
	rt := &iterateProgressRuntime{}
	results, err := IterateStepResultsWithProgress(
		context.Background(),
		rt,
		items,
		func(_ context.Context, item testItem) StepResult[testItem] {
			return Succeeded(item)
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if rt.progressCalls != 1 || rt.lastDone != 2 || rt.lastTotal != 2 {
		t.Fatalf("unexpected progress calls: calls=%d done=%d total=%d", rt.progressCalls, rt.lastDone, rt.lastTotal)
	}
}
