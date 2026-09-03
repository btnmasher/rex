package sqlite

import (
	"bytes"
	"context"
	"testing"

	"github.com/btnmasher/rex/jobruntime/jobstore"
)

func TestStoreRunsLifecycleAndPersistsCheckpoint(t *testing.T) {
	path := t.TempDir() + "/jobs.sqlite"
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run, err := store.StartOrGetRun(ctx, &jobstore.StartRunRequest{JobID: "job", JobKind: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := jobstore.Checkpoint{Namespace: "test-v1", Value: []byte(`{"offset":1}`)}
	if err := store.SaveRunCheckpoint(ctx, run.RunID, checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertItems(ctx, run.RunID, []jobstore.WorkItem{{ItemID: "1", PayloadJSON: []byte(`{"value":1}`)}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimItems(ctx, jobstore.ClaimRequest{RunID: run.RunID})
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("claim = %+v, err = %v", claim, err)
	}
	if err := store.MarkItemCompleted(ctx, &jobstore.SuccessRecord{
		RunID:        run.RunID,
		ItemID:       "1",
		AttemptCount: claim.Items[0].AttemptCount,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRunCompleted(ctx, run.RunID, map[string]any{"done": true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.GetRunCheckpoint(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Namespace != checkpoint.Namespace || !bytes.Equal(got.Value, checkpoint.Value) {
		t.Fatalf("checkpoint = %+v", got)
	}
	status, err := reopened.GetRunStatus(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "COMPLETED" {
		t.Fatalf("status = %q", status.Status)
	}
}

func TestStoreRetryGroupAndAttemptFencing(t *testing.T) {
	store, err := NewStore(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()
	run, err := store.StartOrGetRun(ctx, &jobstore.StartRunRequest{JobID: "job", JobKind: "kind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertItems(ctx, run.RunID, []jobstore.WorkItem{{ItemID: "1", PayloadJSON: []byte(`{"value":1}`)}}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimItems(ctx, jobstore.ClaimRequest{RunID: run.RunID})
	if err != nil || len(claim.Items) != 1 {
		t.Fatalf("claim = %+v, err = %v", claim, err)
	}
	claimed := claim.Items[0]
	if err := store.MarkItemFailedFinal(ctx, jobstore.FailureRecord{
		RunID:        run.RunID,
		ItemID:       claimed.ItemID,
		AttemptCount: claimed.AttemptCount,
		ErrorMsg:     "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkItemCompleted(ctx, &jobstore.SuccessRecord{
		RunID:        run.RunID,
		ItemID:       claimed.ItemID,
		AttemptCount: claimed.AttemptCount,
	}); err == nil {
		t.Fatal("expected stale completion to fail")
	}

	group, err := store.CreateRetryGroup(ctx, &jobstore.CreateRetryGroupRequest{
		JobID:       "job",
		JobKind:     "kind",
		SourceRunID: run.RunID,
		ItemIDs:     []jobstore.ItemID{claimed.ItemID},
	})
	if err != nil {
		t.Fatal(err)
	}
	retryRun, err := store.StartRetryGroup(ctx, group.RetryGroupID)
	if err != nil {
		t.Fatal(err)
	}
	retryClaim, err := store.ClaimItems(ctx, jobstore.ClaimRequest{RunID: retryRun.RunID, RetryGroupID: &group.RetryGroupID})
	if err != nil || len(retryClaim.Items) != 1 {
		t.Fatalf("retry claim = %+v, err = %v", retryClaim, err)
	}
	if err := store.MarkItemCompleted(ctx, &jobstore.SuccessRecord{
		RunID:        retryRun.RunID,
		ItemID:       retryClaim.Items[0].ItemID,
		AttemptCount: retryClaim.Items[0].AttemptCount,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRetryGroup(ctx, group.RetryGroupID); err != nil {
		t.Fatal(err)
	}
}
