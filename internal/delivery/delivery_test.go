package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
)

type testEnricher struct {
	view *enrichment.Context
}

func (e testEnricher) Enrich(context.Context, *enrichment.Envelope) (*enrichment.Context, error) {
	return e.view, nil
}

type testRouter struct{}

func (testRouter) PostRoute(_ *enrichment.Context, candidates []string) []string {
	return append([]string(nil), candidates...)
}

type testAdapter struct {
	mu       sync.Mutex
	statuses map[string]Outcome
	calls    []string
	panicID  string
}

func (a *testAdapter) Deliver(_ context.Context, _ *enrichment.Context, targetID string) Outcome {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, targetID)
	if targetID == a.panicID {
		panic("test adapter panic")
	}
	return a.statuses[targetID]
}

func (a *testAdapter) callsSnapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

type testTerminalRecorder struct {
	mu     sync.Mutex
	failed []string
	reason error
	count  int
}

func (r *testTerminalRecorder) RecordTerminalFailure(_ context.Context, _ *enrichment.Context, destinations []string, reason error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = append([]string(nil), destinations...)
	r.reason = reason
	r.count++
	return nil
}

func testWorker(t *testing.T, adapter Adapter, recorder TerminalRecorder) (*Worker, *notificationstate.MemoryStore) {
	t.Helper()
	store := notificationstate.NewMemoryStore()
	event := notifications.Event{NotificationID: 1, NotificationType: "StructureUnderAttack", AlertType: "structures.combat.under_attack"}
	view := &enrichment.Context{Event: event}
	worker, err := New(&Config{
		Store:          store,
		Enricher:       testEnricher{view: view},
		Router:         testRouter{},
		Adapter:        adapter,
		TerminalRecord: recorder,
		Now:            func() time.Time { return time.Unix(100, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("new worker: %v", err)
	}
	return worker, store
}

func testEnqueueRequest(t *testing.T) *EnqueueRequest {
	t.Helper()
	notification := esi.Notification{
		ID:        1,
		Type:      "StructureUnderAttack",
		Timestamp: time.Unix(99, 0).UTC(),
		Text:      "structure_id: 123456789",
	}
	payload, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	return &EnqueueRequest{
		Envelope: enrichment.Envelope{
			Corporation:         testCorporation(),
			CharacterID:         "2",
			RawNotificationJSON: payload,
			Event:               notifications.Event{NotificationID: 1, NotificationType: notification.Type, AlertType: "structures.combat.under_attack"},
		},
		DestinationIDs: []string{"a", "b"},
		Cursor: notificationstate.Cursor{Streams: map[string]notificationstate.Stream{
			"2": {LastTimestamp: time.Unix(99, 0).UTC(), SeenAtCursor: map[int64]struct{}{1: {}}},
		}},
	}
}

func testCorporation() authnextdb.Corporation {
	return authnextdb.Corporation{ID: "100", Name: "Test Corporation", Ticker: "TEST"}
}

func TestWorkerRetriesOnlyFailedTargets(t *testing.T) {
	adapter := &testAdapter{statuses: map[string]Outcome{
		"a": {Status: OutcomeAccepted},
		"b": {Status: OutcomeRetryable, RetryAfter: time.Second, Err: errors.New("rate limited")},
	}}
	worker, store := testWorker(t, adapter, nil)
	request := testEnqueueRequest(t)
	claimed, err := worker.Enqueue(context.Background(), request)
	if err != nil || !claimed {
		t.Fatalf("enqueue: claimed=%t err=%v", claimed, err)
	}
	worker.process(context.Background(), duePending(t, store))

	pending := pendingAt(t, store, time.Unix(100, 0).UTC().Add(time.Hour))
	if len(pending) != 1 || len(pending[0].FailedDestinationIDs) != 1 || pending[0].FailedDestinationIDs[0] != "b" {
		t.Fatalf("unexpected pending retry: %#v", pending)
	}
	if len(pending[0].DeliveredDestinationIDs) != 1 || pending[0].DeliveredDestinationIDs[0] != "a" {
		t.Fatalf("accepted destination was not durably tracked: %#v", pending[0].DeliveredDestinationIDs)
	}
	pending[0].NextRetryAt = time.Unix(100, 0).UTC()
	if err := store.ReschedulePending(context.Background(), &pending[0]); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	adapter.statuses["b"] = Outcome{Status: OutcomeAccepted}
	worker.process(context.Background(), duePending(t, store))

	calls := adapter.callsSnapshot()
	if len(calls) != 3 || calls[0] != "a" || calls[1] != "b" || calls[2] != "b" {
		t.Fatalf("delivery calls = %#v, want a,b,b", calls)
	}
	if pending := pendingAt(t, store, time.Unix(100, 0).UTC().Add(time.Hour)); len(pending) != 0 {
		t.Fatalf("successful retry remains queued: %#v", pending)
	}
}

func TestWorkerIsolatesDestinationAdapterPanic(t *testing.T) {
	adapter := &testAdapter{
		statuses: map[string]Outcome{"a": {Status: OutcomeAccepted}, "b": {Status: OutcomeAccepted}},
		panicID:  "b",
	}
	worker, store := testWorker(t, adapter, nil)
	if claimed, err := worker.Enqueue(context.Background(), testEnqueueRequest(t)); err != nil || !claimed {
		t.Fatalf("enqueue: claimed=%t err=%v", claimed, err)
	}
	worker.process(context.Background(), duePending(t, store))
	pending := duePending(t, store)
	pending.NextRetryAt = time.Unix(100, 0).UTC()
	adapter.panicID = ""
	if err := store.ReschedulePending(context.Background(), pending); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	worker.process(context.Background(), duePending(t, store))
	if calls := adapter.callsSnapshot(); len(calls) != 3 || calls[0] != "a" || calls[1] != "b" || calls[2] != "b" {
		t.Fatalf("delivery calls = %#v, want a,b,b", calls)
	}
}

func TestWorkerRecordsPermanentFailureAndDeletesPending(t *testing.T) {
	adapter := &testAdapter{statuses: map[string]Outcome{
		"a": {Status: OutcomePermanent, Err: errors.New("invalid webhook")},
		"b": {Status: OutcomeAccepted},
	}}
	recorder := &testTerminalRecorder{}
	worker, store := testWorker(t, adapter, recorder)
	if claimed, err := worker.Enqueue(context.Background(), testEnqueueRequest(t)); err != nil || !claimed {
		t.Fatalf("enqueue: claimed=%t err=%v", claimed, err)
	}
	worker.process(context.Background(), duePending(t, store))
	if pending := pendingAt(t, store, time.Unix(100, 0).UTC().Add(time.Hour)); len(pending) != 0 {
		t.Fatalf("permanent failure remains queued: %#v", pending)
	}
	if recorder.count != 1 || len(recorder.failed) != 1 || recorder.failed[0] != "a" {
		t.Fatalf("terminal record = %+v", recorder)
	}
}

func TestWorkerRecordsAllDestinationsWhenRetryExpires(t *testing.T) {
	recorder := &testTerminalRecorder{}
	worker, store := testWorker(t, &testAdapter{statuses: map[string]Outcome{}}, recorder)
	if claimed, err := worker.Enqueue(context.Background(), testEnqueueRequest(t)); err != nil || !claimed {
		t.Fatalf("enqueue: claimed=%t err=%v", claimed, err)
	}
	pending := duePending(t, store)
	pending.CreatedAt = time.Unix(0, 0).UTC()
	worker.process(context.Background(), pending)
	if recorder.count != 1 || len(recorder.failed) != 2 || recorder.failed[0] != "a" || recorder.failed[1] != "b" {
		t.Fatalf("terminal record = %+v, want both destinations", recorder)
	}
	if pending := pendingAt(t, store, time.Unix(100, 0).UTC().Add(time.Hour)); len(pending) != 0 {
		t.Fatalf("expired retry remains queued: %#v", pending)
	}
}

func TestWorkerDoesNotProcessAfterCancellation(t *testing.T) {
	adapter := &testAdapter{statuses: map[string]Outcome{"a": {Status: OutcomeAccepted}, "b": {Status: OutcomeAccepted}}}
	worker, store := testWorker(t, adapter, nil)
	if claimed, err := worker.Enqueue(context.Background(), testEnqueueRequest(t)); err != nil || !claimed {
		t.Fatalf("enqueue: claimed=%t err=%v", claimed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.process(ctx, duePending(t, store))
	if calls := adapter.callsSnapshot(); len(calls) != 0 {
		t.Fatalf("canceled worker delivered to %#v", calls)
	}
}

func duePending(t *testing.T, store *notificationstate.MemoryStore) *notificationstate.PendingNotification {
	t.Helper()
	items := pendingAt(t, store, time.Unix(100, 0).UTC().Add(time.Hour))
	if len(items) != 1 {
		t.Fatalf("pending items = %d, want 1", len(items))
	}
	return &items[0]
}

func pendingAt(t *testing.T, store *notificationstate.MemoryStore, now time.Time) []notificationstate.PendingNotification {
	t.Helper()
	items, err := store.ListPending(context.Background(), 10, now)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	return items
}
