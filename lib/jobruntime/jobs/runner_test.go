package jobs

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

type testItem struct {
	ItemMeta
}

type testCodec struct{}

type noopProducer struct{}

type panicProducer struct{}

func (noopProducer) Produce(context.Context, ProduceRequest) (ProduceResult, error) {
	return ProduceResult{Done: true}, nil
}

func (panicProducer) Produce(context.Context, ProduceRequest) (ProduceResult, error) {
	panic("producer panic")
}

func (testCodec) DecodeItem(meta ItemMeta, _, _ []byte) (testItem, error) {
	return testItem{ItemMeta: meta}, nil
}

func (testCodec) EncodeResult(testItem) ([]byte, error) { return []byte(`{}`), nil }

type fakeJobsStore struct {
	claimResult    jobstore.ClaimResult
	claimErr       error
	retryRun       jobstore.RetryGroupRun
	reopenErr      error
	reopenCanceled bool

	markRetryCalls []jobstore.RetryRecord
	markFailCalls  []jobstore.FailureRecord
	markRunIDs     []string
}

func (s *fakeJobsStore) StartOrGetRun(context.Context, *jobstore.StartRunRequest) (jobstore.StartRunResult, error) {
	return jobstore.StartRunResult{RunID: "run-1", SlotTS: time.Now().UTC()}, nil
}
func (s *fakeJobsStore) MarkRunCompleted(context.Context, string, map[string]any) error { return nil }
func (s *fakeJobsStore) MarkRunFailed(_ context.Context, runID, _ string) error {
	s.markRunIDs = append(s.markRunIDs, runID)
	return nil
}
func (s *fakeJobsStore) Heartbeat(context.Context, string) error { return nil }
func (s *fakeJobsStore) GetRunCheckpoint(context.Context, string) (jobstore.Checkpoint, error) {
	return jobstore.Checkpoint{}, nil
}
func (s *fakeJobsStore) SaveRunCheckpoint(context.Context, string, jobstore.Checkpoint) error {
	return nil
}
func (s *fakeJobsStore) InsertItems(context.Context, string, []jobstore.WorkItem) error { return nil }
func (s *fakeJobsStore) ClaimItems(context.Context, jobstore.ClaimRequest) (jobstore.ClaimResult, error) {
	if s.claimErr != nil {
		return jobstore.ClaimResult{}, s.claimErr
	}
	return s.claimResult, nil
}
func (s *fakeJobsStore) MarkItemCompleted(context.Context, *jobstore.SuccessRecord) error { return nil }
func (s *fakeJobsStore) MarkItemRetry(_ context.Context, rec *jobstore.RetryRecord) error {
	s.markRetryCalls = append(s.markRetryCalls, *rec)
	return nil
}
func (s *fakeJobsStore) MarkItemFailedFinal(_ context.Context, rec jobstore.FailureRecord) error {
	s.markFailCalls = append(s.markFailCalls, rec)
	return nil
}
func (s *fakeJobsStore) CountItemsByState(context.Context, string) (map[string]int, error) {
	return map[string]int{}, nil
}
func (s *fakeJobsStore) RecordEvent(context.Context, *jobstore.EventRecord) error { return nil }
func (s *fakeJobsStore) CreateRetryGroup(context.Context, *jobstore.CreateRetryGroupRequest) (jobstore.CreateRetryGroupResult, error) {
	return jobstore.CreateRetryGroupResult{}, nil
}
func (s *fakeJobsStore) StartRetryGroup(context.Context, string) (jobstore.RetryGroupRun, error) {
	return s.retryRun, nil
}
func (s *fakeJobsStore) CompleteRetryGroup(context.Context, string) error { return nil }
func (s *fakeJobsStore) ReopenRetryGroup(ctx context.Context, _ string) error {
	s.reopenCanceled = ctx.Err() != nil
	return s.reopenErr
}
func (s *fakeJobsStore) CancelRun(context.Context, string, string) error { return nil }
func (s *fakeJobsStore) GetRunStatus(context.Context, string) (jobstore.RunStatus, error) {
	return jobstore.RunStatus{}, nil
}
func (s *fakeJobsStore) ListRunStatuses(context.Context, string, int) ([]jobstore.RunStatus, error) {
	return nil, nil
}

func TestRegisterStepValidation(t *testing.T) {
	r, err := NewRunner(&fakeJobsStore{}, testCodec{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	if err := r.RegisterStep(StepDefinition[testItem]{}); err == nil {
		t.Fatalf("expected missing-name error")
	}

	if err := r.RegisterStep(StepDefinition[testItem]{Name: "A"}); err == nil {
		t.Fatalf("expected missing-delegate error")
	}

	okStep := StepDefinition[testItem]{
		Name:     "A",
		Delegate: func(context.Context, StepRuntime, []testItem) ([]StepResult[testItem], error) { return nil, nil },
	}
	if err := r.RegisterStep(okStep); err != nil {
		t.Fatalf("register step: %v", err)
	}
	if err := r.RegisterStep(okStep); err == nil {
		t.Fatalf("expected duplicate-name error")
	}
}

func TestRunOnceReturnsPipelineErrorOnClaimFailure(t *testing.T) {
	store := &fakeJobsStore{claimErr: errors.New("claim failed")}
	r, err := NewRunner(store, testCodec{}, WithProducer(noopProducer{}))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	err = r.RunOnce(context.Background(), "run-1")
	if err == nil {
		t.Fatalf("expected error")
	}
	var pe *PipelineError
	if !errors.As(err, &pe) || pe.Stage != "ITEM_CLAIM" {
		t.Fatalf("expected ITEM_CLAIM PipelineError, got %v", err)
	}
}

func TestRunOnceRecoversProducerPanic(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{}, WithProducer(panicProducer{}))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	err = r.RunOnce(context.Background(), "run-1")
	if err == nil || !strings.Contains(err.Error(), "job runtime recovered panic") {
		t.Fatalf("expected recovered producer panic, got %v", err)
	}
	if len(store.markRunIDs) != 1 || store.markRunIDs[0] != "run-1" {
		t.Fatalf("expected panicked run to be marked failed, got %+v", store.markRunIDs)
	}
}

func TestRunRetryGroupReopensWithIndependentRecoveryContext(t *testing.T) {
	store := &fakeJobsStore{
		claimErr: errors.New("claim failed"),
		retryRun: jobstore.RetryGroupRun{RunID: "run-1", JobID: "job-1", JobKind: "kind-1"},
	}
	r, err := NewRunner(store, testCodec{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.RunRetryGroup(ctx, "rg-1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	if store.reopenCanceled {
		t.Fatal("retry group recovery used the canceled request context")
	}
}

func TestMarkRetryFinalizesAfterMaxAttempts(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{}, WithMaxAttempts(2))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	item := testItem{ItemID: "42", AttemptCount: 2}
	mustFail, err := r.markRetry(context.Background(), "run-1", item, &StepResult[testItem]{
		Status: ItemStatusRetry,
		Error:  "temporary failure",
	}, true)
	if err != nil {
		t.Fatalf("markRetry failed: %v", err)
	}
	if !mustFail {
		t.Fatalf("expected mustFail=true when max attempts reached")
	}
	if len(store.markFailCalls) != 1 {
		t.Fatalf("expected one final fail record, got %d", len(store.markFailCalls))
	}
}

func TestExecuteItemWorkflowFailsWhenDelegateOmitsResult(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	err = r.RegisterStep(StepDefinition[testItem]{
		Name:     "STEP_A",
		Critical: true,
		Delegate: func(context.Context, StepRuntime, []testItem) ([]StepResult[testItem], error) {
			return []StepResult[testItem]{}, nil
		},
	})
	if err != nil {
		t.Fatalf("register step: %v", err)
	}

	item := testItem{ItemID: "abc", AttemptCount: 1}
	mustFail, err := r.executeItemWorkflow(context.Background(), &JobMeta{
		JobID:   "job-1",
		JobKind: "kind-1",
		RunID:   "run-1",
	}, nil, item)
	if err != nil {
		t.Fatalf("executeItemWorkflow: %v", err)
	}
	if !mustFail {
		t.Fatalf("expected mustFail=true")
	}
	if len(store.markFailCalls) != 1 {
		t.Fatalf("expected one mark-fail call, got %d", len(store.markFailCalls))
	}
	if !strings.Contains(store.markFailCalls[0].ErrorMsg, "delegate did not return result") {
		t.Fatalf("unexpected fail error message: %q", store.markFailCalls[0].ErrorMsg)
	}
}

func TestProcessClaimedItemsKeepsFailuresIndependent(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{}, WithMaxAttempts(3))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	var siblingContextCanceled atomic.Bool
	if err := r.RegisterStep(StepDefinition[testItem]{
		Name: "STEP_A",
		Delegate: func(ctx context.Context, _ StepRuntime, items []testItem) ([]StepResult[testItem], error) {
			if items[0].ID() == "failed" {
				return nil, errors.New("delegate failed")
			}
			if ctx.Err() != nil {
				siblingContextCanceled.Store(true)
			}
			return []StepResult[testItem]{Succeeded(items[0])}, nil
		},
	}); err != nil {
		t.Fatalf("register step: %v", err)
	}

	hardFailures, err := r.processClaimedItems(context.Background(), &JobMeta{RunID: "run-1"}, nil, []jobstore.WorkItem{
		{ItemID: "failed", AttemptCount: 1},
		{ItemID: "sibling", AttemptCount: 1},
	})
	if err != nil {
		t.Fatalf("process claimed items: %v", err)
	}
	if hardFailures != 0 || siblingContextCanceled.Load() {
		t.Fatalf("unexpected sibling cancellation or hard failure: hard_failures=%d canceled=%t", hardFailures, siblingContextCanceled.Load())
	}
	if len(store.markRetryCalls) != 1 || store.markRetryCalls[0].ItemID != "failed" {
		t.Fatalf("expected failed item to be queued for retry, got %+v", store.markRetryCalls)
	}
}

func TestProcessClaimedItemsRecoversStepPanicAndRetriesItem(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{}, WithMaxAttempts(3))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	if err := r.RegisterStep(StepDefinition[testItem]{
		Name: "STEP_A",
		Delegate: func(context.Context, StepRuntime, []testItem) ([]StepResult[testItem], error) {
			panic("step panic")
		},
	}); err != nil {
		t.Fatalf("register step: %v", err)
	}

	hardFailures, err := r.processClaimedItems(context.Background(), &JobMeta{RunID: "run-1"}, nil, []jobstore.WorkItem{
		{ItemID: "panicked", AttemptCount: 1},
	})
	if err != nil || hardFailures != 0 {
		t.Fatalf("expected panic to become an item retry, hard_failures=%d err=%v", hardFailures, err)
	}
	if len(store.markRetryCalls) != 1 || store.markRetryCalls[0].ItemID != "panicked" {
		t.Fatalf("expected panicked item to be queued for retry, got %+v", store.markRetryCalls)
	}
}

func TestApplyStepResultCanceledMarksFailed(t *testing.T) {
	store := &fakeJobsStore{}
	r, err := NewRunner(store, testCodec{})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	item := testItem{ItemID: "u1"}
	done, mustFail, err := r.applyStepResult(
		context.Background(),
		&JobMeta{RunID: "run-1"},
		&StepDefinition[testItem]{Name: "STEP_A"},
		item,
		&StepResult[testItem]{Item: item, Status: ItemStatusCanceled},
		true,
	)
	if err != nil {
		t.Fatalf("applyStepResult: %v", err)
	}
	if !done || !mustFail {
		t.Fatalf("expected done=true and mustFail=true")
	}
	if len(store.markFailCalls) != 1 || store.markFailCalls[0].ErrorMsg != "canceled" {
		t.Fatalf("expected canceled mark-fail record, got %+v", store.markFailCalls)
	}
}
