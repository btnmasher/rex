package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

func startMemoryRun(t *testing.T, store *Store) string {
	t.Helper()
	result, err := store.StartOrGetRun(context.Background(), &jobstore.StartRunRequest{JobID: "job", JobKind: "kind"})
	if err != nil {
		t.Fatalf("start run: %v", err)
	}
	return result.RunID
}

func TestClaimReportsDelayedRetryAsPending(t *testing.T) {
	store := NewStore()
	runID := startMemoryRun(t, store)
	if err := store.InsertItems(context.Background(), runID, []jobstore.WorkItem{{ItemID: "1", PayloadJSON: []byte(`{}`)}}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	claim, err := store.ClaimItems(context.Background(), jobstore.ClaimRequest{RunID: runID})
	if err != nil {
		t.Fatalf("claim item: %v", err)
	}
	if err := store.MarkItemRetry(context.Background(), &jobstore.RetryRecord{
		RunID:        runID,
		ItemID:       claim.Items[0].ItemID,
		AttemptCount: claim.Items[0].AttemptCount,
		RetryAt:      time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatalf("mark retry: %v", err)
	}
	claim, err = store.ClaimItems(context.Background(), jobstore.ClaimRequest{RunID: runID})
	if err != nil || !claim.Pending || claim.NextRetryAt == nil {
		t.Fatalf("expected delayed retry to remain pending, result=%+v err=%v", claim, err)
	}
}

func TestStaleRunAndInflightItemCanBeRecovered(t *testing.T) {
	store := NewStore()
	runID := startMemoryRun(t, store)
	store.runs[runID].status.UpdatedAt = time.Now().Add(-staleRunTimeout - time.Second)
	newRun, err := store.StartOrGetRun(context.Background(), &jobstore.StartRunRequest{JobID: "job", JobKind: "kind"})
	if err != nil || newRun.ExistingRunning || newRun.RunID != runID {
		t.Fatalf("expected stale run recovery in place, result=%+v err=%v", newRun, err)
	}
	if err := store.InsertItems(context.Background(), newRun.RunID, []jobstore.WorkItem{{ItemID: "1"}}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	firstClaim, err := store.ClaimItems(context.Background(), jobstore.ClaimRequest{RunID: newRun.RunID})
	if err != nil {
		t.Fatalf("claim item: %v", err)
	}
	store.runs[newRun.RunID].items["1"].updatedAt = time.Now().Add(-inflightTimeout - time.Second)
	claim, err := store.ClaimItems(context.Background(), jobstore.ClaimRequest{RunID: newRun.RunID})
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("expected stale inflight recovery, result=%+v err=%v", claim, err)
	}
	if err := store.MarkItemCompleted(context.Background(), &jobstore.SuccessRecord{
		RunID:        newRun.RunID,
		ItemID:       firstClaim.Items[0].ItemID,
		AttemptCount: firstClaim.Items[0].AttemptCount,
	}); !errors.Is(err, jobstore.ErrStaleClaim) {
		t.Fatalf("expected stale worker fencing error, got %v", err)
	}
}

func TestSaveRunCheckpointCopiesValue(t *testing.T) {
	store := NewStore()
	runID := startMemoryRun(t, store)
	value := []byte("checkpoint")
	checkpoint := jobstore.Checkpoint{Namespace: "test", Value: value}
	if err := store.SaveRunCheckpoint(context.Background(), runID, checkpoint); err != nil {
		t.Fatalf("save checkpoint: %v", err)
	}
	value[0] = 'X'
	got, err := store.GetRunCheckpoint(context.Background(), runID)
	if err != nil {
		t.Fatalf("get checkpoint: %v", err)
	}
	if got.Namespace != "test" || string(got.Value) != "checkpoint" {
		t.Fatalf("checkpoint = %+v", got)
	}
}

func TestInsertItemsIsIdempotentForMatchingPayload(t *testing.T) {
	store := NewStore()
	runID := startMemoryRun(t, store)
	item := jobstore.WorkItem{ItemID: "1", PayloadJSON: []byte(`{"value":1}`)}
	if err := store.InsertItems(context.Background(), runID, []jobstore.WorkItem{item}); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	if err := store.InsertItems(context.Background(), runID, []jobstore.WorkItem{item}); err != nil {
		t.Fatalf("repeat insert item: %v", err)
	}
	claim, err := store.ClaimItems(context.Background(), jobstore.ClaimRequest{RunID: runID})
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("expected one idempotent item, result=%+v err=%v", claim, err)
	}
}

func TestInsertItemsRejectsConflictingDuplicateBatch(t *testing.T) {
	store := NewStore()
	runID := startMemoryRun(t, store)

	err := store.InsertItems(context.Background(), runID, []jobstore.WorkItem{
		{ItemID: "1", PayloadJSON: []byte(`{"value":1}`)},
		{ItemID: "1", PayloadJSON: []byte(`{"value":2}`)},
	})
	if err == nil {
		t.Fatal("expected conflicting duplicate batch to fail")
	}
}
