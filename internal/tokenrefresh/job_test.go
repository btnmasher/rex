package tokenrefresh

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btnmasher/rex/jobruntime/jobstore/memory"
	jobsqlite "github.com/btnmasher/rex/jobruntime/jobstore/sqlite"
)

type testSource struct {
	listCalls   atomic.Int32
	corporation []string
	listErr     error
}

func (s *testSource) ListCorporations(context.Context) ([]string, error) {
	s.listCalls.Add(1)
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.corporation, nil
}

type testTokenStore struct {
	active       atomic.Int32
	maxActive    atomic.Int32
	refreshCalls atomic.Int32
	mu           sync.Mutex
	eligible     []string
	refreshIDs   []string
}

func (s *testTokenStore) SetEligibleCorporations(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eligible = append([]string(nil), ids...)
}

func (s *testTokenStore) RefreshCorporationIfDue(_ context.Context, corporationID string, _ time.Duration) (bool, error) {
	active := s.active.Add(1)
	for {
		current := s.maxActive.Load()
		if active <= current || s.maxActive.CompareAndSwap(current, active) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	s.active.Add(-1)
	s.refreshCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshIDs = append(s.refreshIDs, corporationID)
	return true, nil
}

func (s *testTokenStore) HasUsableTokens() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.refreshIDs) > 0
}

func (s *testTokenStore) TokenCount(corporationID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.Contains(s.refreshIDs, corporationID) {
		return 1
	}
	return 0
}

func TestJobFetchesCorporationsWithBoundedConcurrency(t *testing.T) {
	source := &testSource{corporation: []string{"3", "1", "2", "4"}}
	tokens := &testTokenStore{}
	store := memory.NewStore()
	job, err := New(store, source, tokens, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatalf("run token refresh job: %v", err)
	}
	if source.listCalls.Load() != 1 || tokens.refreshCalls.Load() != 4 {
		t.Fatalf("source/store calls: list=%d refresh=%d", source.listCalls.Load(), tokens.refreshCalls.Load())
	}
	select {
	case <-job.Ready():
	default:
		t.Fatal("successful refresh did not signal readiness")
	}
	if tokens.maxActive.Load() > 2 {
		t.Fatalf("maximum refresh concurrency = %d", tokens.maxActive.Load())
	}
	tokens.mu.Lock()
	defer tokens.mu.Unlock()
	if len(tokens.eligible) != 4 || len(tokens.refreshIDs) != 4 {
		t.Fatalf("token store state: eligible=%v refreshed=%v", tokens.eligible, tokens.refreshIDs)
	}
}

func TestJobDoesNotSignalReadyWhenCorporationListingFails(t *testing.T) {
	source := &testSource{listErr: errors.New("auth-next unavailable")}
	tokens := &testTokenStore{}
	job, err := New(memory.NewStore(), source, tokens, 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Run(context.Background()); err == nil {
		t.Fatal("expected refresh error")
	}
	select {
	case <-job.Ready():
		t.Fatal("failed refresh signaled readiness")
	default:
	}
	if tokens.refreshCalls.Load() != 0 {
		t.Fatalf("refreshed corporation tokens after listing failure: %d", tokens.refreshCalls.Load())
	}
}

func TestJobQueuesCorporationIDsInDurableStore(t *testing.T) {
	store, err := jobsqlite.NewStore(context.Background(), filepath.Join(t.TempDir(), "jobruntime.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	source := &testSource{corporation: []string{"1018389948"}}
	tokens := &testTokenStore{}
	job, err := New(store, source, tokens, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Run(context.Background()); err != nil {
		t.Fatalf("run token refresh job with durable store: %v", err)
	}
	if source.listCalls.Load() != 1 || tokens.refreshCalls.Load() != 1 {
		t.Fatalf("source/store calls: list=%d refresh=%d", source.listCalls.Load(), tokens.refreshCalls.Load())
	}
}
