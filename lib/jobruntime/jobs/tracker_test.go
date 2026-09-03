package jobs

import (
	"context"
	"log/slog"
	"testing"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

type captureEventStore struct {
	last *jobstore.EventRecord
}

type captureProgressReporter struct {
	last *ProgressReport
}

func (p *captureProgressReporter) ReportProgress(
	_ context.Context,
	report ProgressReport, //nolint:gocritic // interface contract uses a value report
) {
	cp := report
	p.last = &cp
}

func (s *captureEventStore) StartOrGetRun(context.Context, *jobstore.StartRunRequest) (jobstore.StartRunResult, error) {
	return jobstore.StartRunResult{}, nil
}
func (s *captureEventStore) MarkRunCompleted(context.Context, string, map[string]any) error {
	return nil
}
func (s *captureEventStore) MarkRunFailed(context.Context, string, string) error { return nil }
func (s *captureEventStore) Heartbeat(context.Context, string) error             { return nil }
func (s *captureEventStore) GetRunCheckpoint(context.Context, string) (jobstore.Checkpoint, error) {
	return jobstore.Checkpoint{}, nil
}
func (s *captureEventStore) SaveRunCheckpoint(context.Context, string, jobstore.Checkpoint) error {
	return nil
}
func (s *captureEventStore) InsertItems(context.Context, string, []jobstore.WorkItem) error {
	return nil
}
func (s *captureEventStore) ClaimItems(context.Context, jobstore.ClaimRequest) (jobstore.ClaimResult, error) {
	return jobstore.ClaimResult{}, nil
}
func (s *captureEventStore) MarkItemCompleted(context.Context, *jobstore.SuccessRecord) error {
	return nil
}
func (s *captureEventStore) MarkItemRetry(context.Context, *jobstore.RetryRecord) error { return nil }
func (s *captureEventStore) MarkItemFailedFinal(context.Context, jobstore.FailureRecord) error {
	return nil
}
func (s *captureEventStore) CountItemsByState(context.Context, string) (map[string]int, error) {
	return map[string]int{}, nil
}
func (s *captureEventStore) RecordEvent(_ context.Context, rec *jobstore.EventRecord) error {
	s.last = rec
	return nil
}
func (s *captureEventStore) CreateRetryGroup(context.Context, *jobstore.CreateRetryGroupRequest) (jobstore.CreateRetryGroupResult, error) {
	return jobstore.CreateRetryGroupResult{}, nil
}
func (s *captureEventStore) StartRetryGroup(context.Context, string) (jobstore.RetryGroupRun, error) {
	return jobstore.RetryGroupRun{}, nil
}
func (s *captureEventStore) CompleteRetryGroup(context.Context, string) error { return nil }
func (s *captureEventStore) ReopenRetryGroup(context.Context, string) error   { return nil }
func (s *captureEventStore) CancelRun(context.Context, string, string) error  { return nil }
func (s *captureEventStore) GetRunStatus(context.Context, string) (jobstore.RunStatus, error) {
	return jobstore.RunStatus{}, nil
}
func (s *captureEventStore) ListRunStatuses(context.Context, string, int) ([]jobstore.RunStatus, error) {
	return nil, nil
}

func TestStepRuntimeProgressPersistsStructuredMeta(t *testing.T) {
	store := &captureEventStore{}
	reporter := &captureProgressReporter{}
	rt := newStepRuntime(&stepRuntimeParams{
		store:            store,
		runID:            "run-1",
		jobID:            "job-1",
		jobKind:          "kind-1",
		step:             "STEP_A",
		progressReporter: reporter,
	})

	err := rt.Progress(context.Background(), 3, 7,
		slog.String("status", "ok"),
		slog.Group("extra", slog.Int("a", 1), slog.String("b", "two")),
	)
	if err != nil {
		t.Fatalf("progress failed: %v", err)
	}
	if store.last != nil {
		t.Fatalf("progress should not be persisted as an event")
	}
	if reporter.last == nil {
		t.Fatalf("expected runtime progress report")
	}
	if reporter.last.RunID != "run-1" || reporter.last.Step != "STEP_A" || reporter.last.Done != 3 || reporter.last.Total != 7 {
		t.Fatalf("unexpected progress report: %+v", reporter.last)
	}
}

func TestAttrsToMetaHandlesPrimitiveKinds(t *testing.T) {
	meta := attrsToMeta([]slog.Attr{
		slog.Bool("b", true),
		slog.Int64("i", 9),
		slog.Uint64("u", 12),
		slog.Float64("f", 1.5),
		slog.String("s", "x"),
	})
	if meta["b"] != true || meta["i"] != int64(9) || meta["u"] != uint64(12) || meta["f"] != 1.5 || meta["s"] != "x" {
		t.Fatalf("unexpected meta conversion: %#v", meta)
	}
}
