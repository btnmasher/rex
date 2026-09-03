package poller

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	alertservice "github.com/btnmasher/rex/internal/alerts"
	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/token"
)

type pollerDatabase struct {
	corporations []authnextdb.Corporation
}

func (d *pollerDatabase) ListNotificationCorporations(context.Context) ([]authnextdb.Corporation, error) {
	return d.corporations, nil
}

type pollerTokens struct {
	next    atomic.Uint64
	refresh atomic.Uint64
}

type expiredTokens struct {
	refresh atomic.Uint64
}

type cooldownTokens struct{}

func (cooldownTokens) HasConfiguredTokens(string) bool { return true }

func (cooldownTokens) NextAccessToken(context.Context, string) (token.Credential, error) {
	return token.Credential{}, token.ErrNotificationCooldown
}

func (cooldownTokens) RefreshCorporation(context.Context, string) error {
	return errors.New("unexpected refresh")
}

func (cooldownTokens) RefreshAccessToken(context.Context, string, string) (token.Credential, error) {
	return token.Credential{}, errors.New("unexpected refresh")
}

func (p *pollerTokens) NextAccessToken(context.Context, string) (token.Credential, error) {
	characterID := "1"
	if p.next.Add(1)%2 == 0 {
		characterID = "2"
	}
	return token.Credential{CharacterID: characterID, AccessToken: "token"}, nil
}

func (*pollerTokens) HasConfiguredTokens(string) bool { return true }

func (*pollerTokens) RefreshCorporation(context.Context, string) error {
	return nil
}

func (p *pollerTokens) RefreshAccessToken(context.Context, string, string) (token.Credential, error) {
	p.refresh.Add(1)
	return token.Credential{CharacterID: "1", AccessToken: "refreshed-token"}, nil
}

func (p *expiredTokens) HasConfiguredTokens(string) bool { return true }

func (p *expiredTokens) NextAccessToken(context.Context, string) (token.Credential, error) {
	if p.refresh.Load() == 0 {
		return token.Credential{}, &token.NoUsableTokenError{
			CorporationID: "100",
			Reason:        "all_tokens_expired",
			TokenCount:    15,
			ExpiredCount:  15,
		}
	}
	return token.Credential{CharacterID: "1", AccessToken: "refreshed-token"}, nil
}

func (p *expiredTokens) RefreshCorporation(context.Context, string) error {
	p.refresh.Add(1)
	return nil
}

func (*expiredTokens) RefreshAccessToken(context.Context, string, string) (token.Credential, error) {
	return token.Credential{}, errors.New("unexpected character refresh")
}

type pollerESI struct {
	characters   []string
	timestamp    time.Time
	streams      map[string][]esi.Notification
	unauthorized atomic.Bool
	mu           sync.Mutex
}

func (e *pollerESI) Notifications(_ context.Context, characterID, _ string) ([]esi.Notification, error) {
	e.mu.Lock()
	e.characters = append(e.characters, characterID)
	e.mu.Unlock()
	if e.unauthorized.Swap(false) {
		return nil, &esi.Error{StatusCode: http.StatusUnauthorized}
	}
	if e.streams != nil {
		return e.streams[characterID], nil
	}
	return []esi.Notification{{ID: 1, Type: "StructureUnderAttack", SenderID: 2, Timestamp: e.timestamp, Text: "structure_id: 123456789"}}, nil
}

func (e *pollerESI) characterSnapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.characters...)
}

func TestPollOnceRefreshesAfterESIAuthenticationFailure(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	esiClient.unauthorized.Store(true)
	tokens := &pollerTokens{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, tokens, esiClient, &pollerAlerts{})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	characters := esiClient.characterSnapshot()
	if tokens.refresh.Load() != 1 || len(characters) != 2 {
		t.Fatalf(
			"expected one access-token refresh and two ESI requests, got refreshes=%d requests=%d",
			tokens.refresh.Load(),
			len(characters),
		)
	}
}

func TestPollOnceRefreshesWhenAllCachedTokensAreExpired(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	tokens := &expiredTokens{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, tokens, esiClient, &pollerAlerts{})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if tokens.refresh.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", tokens.refresh.Load())
	}
	if characters := esiClient.characterSnapshot(); len(characters) != 1 || characters[0] != "1" {
		t.Fatalf("expected one request with refreshed token identity, got %#v", characters)
	}
}

func TestPollOnceSkipsCorporationDuringTokenCooldown(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, cooldownTokens{}, esiClient, &pollerAlerts{})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if characters := esiClient.characterSnapshot(); len(characters) != 0 {
		t.Fatalf("expected cooldown to skip ESI request, got %#v", characters)
	}
}

func TestPollOnceSkipsCorporationWithoutConfiguredTokens(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, emptyTokens{}, esiClient, &pollerAlerts{})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if characters := esiClient.characterSnapshot(); len(characters) != 0 {
		t.Fatalf("expected corporation without configured tokens to be skipped, got %#v", characters)
	}
}

func TestDrainRetriesDropsExpiredAlertAndAdvancesCursor(t *testing.T) {
	stateStore := notificationstate.NewMemoryStore()
	now := time.Now().UTC()
	esiClient := &pollerESI{timestamp: now}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}},
	}, &pollerTokens{}, esiClient, &pollerAlerts{}, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if _, err := service.streamSnapshot(context.Background(), "100", "1"); err != nil {
		t.Fatalf("initialize stream: %v", err)
	}
	pending := &notificationstate.PendingNotification{
		NotificationID:   99,
		CorporationID:    "100",
		CharacterID:      "1",
		NotificationJSON: []byte(`{"notification_id":99,"type":"StructureUnderAttack","timestamp":"2026-09-02T00:00:00Z"}`),
		CreatedAt:        now.Add(-maxNotificationRetryAge - time.Second),
		NextRetryAt:      now.Add(-time.Second),
	}
	if claimed, err := stateStore.ClaimPending(context.Background(), pending); err != nil || !claimed {
		t.Fatalf("claim expired pending: claimed=%t err=%v", claimed, err)
	}
	if err := service.drainRetries(context.Background()); err != nil {
		t.Fatalf("drain expired retry: %v", err)
	}
	remaining, err := stateStore.ListPending(context.Background(), 10, now)
	if err != nil {
		t.Fatalf("list retries: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expired retry remains queued: %#v", remaining)
	}
	cursor, err := stateStore.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if cursor.Streams["1"].LastTimestamp.IsZero() {
		t.Fatal("expired retry did not advance cursor")
	}
}

type emptyTokens struct{}

func (emptyTokens) HasConfiguredTokens(string) bool { return false }

func (emptyTokens) NextAccessToken(context.Context, string) (token.Credential, error) {
	return token.Credential{}, token.ErrNoUsableToken
}

func (emptyTokens) RefreshCorporation(context.Context, string) error {
	return errors.New("unexpected refresh")
}

func (emptyTokens) RefreshAccessToken(context.Context, string, string) (token.Credential, error) {
	return token.Credential{}, errors.New("unexpected refresh")
}

type pollerAlerts struct {
	count    int
	failures int
}

type failingSaveStateStore struct {
	*notificationstate.MemoryStore

	err error
}

func (s *failingSaveStateStore) Save(context.Context, string, notificationstate.Cursor) error {
	return s.err
}

type panicSaveStateStore struct {
	*notificationstate.MemoryStore
}

func (*panicSaveStateStore) Save(context.Context, string, notificationstate.Cursor) error {
	panic("save panic")
}

type failingClaimStateStore struct {
	*notificationstate.MemoryStore

	fail atomic.Bool
}

func (s *failingClaimStateStore) ClaimPending(ctx context.Context, pending *notificationstate.PendingNotification) (bool, error) {
	if s.fail.Swap(false) {
		return false, errors.New("claim failed")
	}
	return s.MemoryStore.ClaimPending(ctx, pending)
}

type failingDeleteStateStore struct {
	*notificationstate.MemoryStore

	fail atomic.Bool
}

func (s *failingDeleteStateStore) DeletePending(ctx context.Context, notificationID int64) error {
	if s.fail.Swap(false) {
		return errors.New("delete failed")
	}
	return s.MemoryStore.DeletePending(ctx, notificationID)
}

func TestAdvanceStreamsDoesNotMutateMemoryWhenSaveFails(t *testing.T) {
	stateStore := &failingSaveStateStore{MemoryStore: notificationstate.NewMemoryStore(), err: errors.New("save failed")}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}},
	}, &pollerTokens{}, &pollerESI{}, &pollerAlerts{}, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	snapshot, err := service.streamSnapshot(context.Background(), "100", "1")
	if err != nil {
		t.Fatalf("initialize stream: %v", err)
	}
	item := esi.Notification{ID: 88, Timestamp: time.Now().UTC()}
	if err := service.advanceStream(context.Background(), "100", "1", &item); err == nil {
		t.Fatal("expected cursor save failure")
	}
	service.mu.Lock()
	inMemoryTimestamp := service.states["100"].streams["1"].lastTimestamp
	service.mu.Unlock()
	if !inMemoryTimestamp.Equal(snapshot.lastTimestamp) {
		t.Fatalf("failed cursor changed in memory: got %v want %v", inMemoryTimestamp, snapshot.lastTimestamp)
	}
	cursor, err := stateStore.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if len(cursor.Streams) != 0 {
		t.Fatalf("failed cursor was persisted in memory: %#v", cursor)
	}
}

func TestPollOnceRecoversFromStateSavePanicWithoutDeadlocking(t *testing.T) {
	stateStore := &panicSaveStateStore{MemoryStore: notificationstate.NewMemoryStore()}
	service, err := NewWithConfig(
		&pollerDatabase{corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}}},
		&pollerTokens{},
		&pollerESI{streams: map[string][]esi.Notification{
			"1": {{ID: 302, Type: "StructureUnderAttack", Timestamp: time.Now().UTC(), Text: "structure_id: 123456789"}},
		}},
		&pollerAlerts{},
		Config{StateStore: stateStore},
	)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err == nil {
		t.Fatal("expected state save panic to be recovered")
	}

	finished := make(chan error, 1)
	go func() { finished <- service.PollOnce(context.Background()) }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("second poll deadlocked after recovered state save panic")
	}
}

func TestProcessNotificationsDropsFutureTimestampRows(t *testing.T) {
	stateStore := notificationstate.NewMemoryStore()
	alertSink := &pollerAlerts{}
	service, err := NewWithConfig(
		&pollerDatabase{corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}}},
		&pollerTokens{},
		&pollerESI{},
		alertSink,
		Config{StateStore: stateStore},
	)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	corporation := authnextdb.Corporation{ID: "100", Name: "Corp"}
	item := esi.Notification{
		ID:        303,
		Type:      "StructureUnderAttack",
		Timestamp: time.Now().UTC().Add(time.Hour),
		Text:      "structure_id: 123456789",
	}
	if err := service.processNotifications(context.Background(), corporation, "1", []esi.Notification{item}); err != nil {
		t.Fatalf("process future notification: %v", err)
	}
	if alertSink.count != 0 {
		t.Fatalf("future notification deliveries = %d, want 0", alertSink.count)
	}
}

func TestProcessEligibleNotificationsStopsBeforeCursorCanSkipClaimFailure(t *testing.T) {
	now := time.Now().UTC()
	stateStore := &failingClaimStateStore{MemoryStore: notificationstate.NewMemoryStore()}
	stateStore.fail.Store(true)
	alertSink := &pollerAlerts{}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}},
	}, &pollerTokens{}, &pollerESI{}, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	corporation := authnextdb.Corporation{ID: "100", Name: "Corp"}
	items := []esi.Notification{
		{ID: 101, Type: "StructureUnderAttack", Timestamp: now.Add(-time.Minute), Text: "structure_id: 123456789"},
		{ID: 102, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"},
	}
	if err := service.processNotifications(context.Background(), corporation, "1", items); err == nil {
		t.Fatal("expected claim failure")
	}
	if alertSink.count != 0 {
		t.Fatalf("later notification was delivered after claim failure: %d", alertSink.count)
	}
	if err := service.processNotifications(context.Background(), corporation, "1", items); err != nil {
		t.Fatalf("retry notifications: %v", err)
	}
	if alertSink.count != 2 {
		t.Fatalf("notifications delivered after retry = %d, want 2", alertSink.count)
	}
}

func TestPollOnceSuppressesRetryAfterCursorAdvanceAndDeleteFailure(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 201, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	stateStore := &failingDeleteStateStore{MemoryStore: notificationstate.NewMemoryStore()}
	stateStore.fail.Store(true)
	alertSink := &pollerAlerts{}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}},
	}, &pollerTokens{}, &pollerESI{streams: map[string][]esi.Notification{"1": {item}}}, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err == nil {
		t.Fatal("expected pending cleanup failure")
	}
	if alertSink.count != 1 {
		t.Fatalf("initial deliveries = %d, want 1", alertSink.count)
	}
	pending, err := stateStore.ListPending(context.Background(), 10, time.Now().UTC().Add(time.Minute))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending retries = %v, err=%v", pending, err)
	}
	pending[0].NextRetryAt = time.Now().UTC().Add(-time.Second)
	if err := stateStore.ReschedulePending(context.Background(), &pending[0]); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("drain cleanup retry: %v", err)
	}
	if alertSink.count != 1 {
		t.Fatalf("deliveries after cleanup retry = %d, want 1", alertSink.count)
	}
}

func TestAdvanceStreamClampsFutureTimestamp(t *testing.T) {
	stateStore := notificationstate.NewMemoryStore()
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp"}},
	}, &pollerTokens{}, &pollerESI{}, &pollerAlerts{}, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	item := esi.Notification{ID: 301, Timestamp: future}
	if _, err := service.streamSnapshot(context.Background(), "100", "1"); err != nil {
		t.Fatalf("initialize stream: %v", err)
	}
	if err := service.advanceStream(context.Background(), "100", "1", &item); err != nil {
		t.Fatalf("advance future notification: %v", err)
	}
	cursor, err := stateStore.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if cursor.Streams["1"].LastTimestamp.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("future timestamp poisoned cursor: %v", cursor.Streams["1"].LastTimestamp)
	}
}

func (a *pollerAlerts) Deliver(context.Context, *alertservice.DeliveryRequest) error {
	a.count++
	if a.failures > 0 {
		a.failures--
		return errors.New("delivery failed")
	}
	return nil
}

func TestPollOnceUsesTokenAndSuppressesDuplicate(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	alertSink := &pollerAlerts{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if alertSink.count != 1 {
		t.Fatalf("expected one delivery, got %d", alertSink.count)
	}
	characters := esiClient.characterSnapshot()
	if len(characters) != 2 || characters[0] != "1" || characters[1] != "2" {
		t.Fatalf("expected token-selected character, got %#v", characters)
	}
}

func TestPollOnceKeepsIndependentCharacterCursors(t *testing.T) {
	now := time.Now().UTC()
	esiClient := &pollerESI{streams: map[string][]esi.Notification{
		"1": {{ID: 10, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}},
		"2": {{ID: 11, Type: "StructureUnderAttack", Timestamp: now.Add(-time.Minute), Text: "structure_id: 123456790"}},
	}}
	alertSink := &pollerAlerts{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if alertSink.count != 2 {
		t.Fatalf("expected both character streams to deliver, got %d", alertSink.count)
	}
}

func TestPollOnceDeduplicatesGlobalNotificationIDAcrossCharacters(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 99, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	esiClient := &pollerESI{streams: map[string][]esi.Notification{"1": {item}, "2": {item}}}
	alertSink := &pollerAlerts{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if alertSink.count != 1 {
		t.Fatalf("expected one delivery for globally unique notification ID, got %d", alertSink.count)
	}
}

func TestPollOnceDeduplicatesNotificationIDAcrossCorporations(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 101, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	esiClient := &pollerESI{streams: map[string][]esi.Notification{"1": {item}, "2": {item}}}
	alertSink := &pollerAlerts{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{
			{ID: "100", Name: "Corp A", Ticker: "A"},
			{ID: "200", Name: "Corp B", Ticker: "B"},
		},
	}, &pollerTokens{}, esiClient, alertSink)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if alertSink.count != 1 {
		t.Fatalf("expected one delivery for globally unique notification ID, got %d", alertSink.count)
	}
}

func TestPollOnceDropsAndMarksMalformedNotifications(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 102, Timestamp: now, Text: "structure_id: 123456789"}
	esiClient := &pollerESI{streams: map[string][]esi.Notification{"1": {item}, "2": {item}}}
	alertSink := &pollerAlerts{}
	service, err := New(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if alertSink.count != 0 {
		t.Fatalf("malformed notification was delivered %d times", alertSink.count)
	}
}

func TestPollOnceRestoresCursorAcrossPollerInstances(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 100, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	stateStore := notificationstate.NewMemoryStore()
	database := &pollerDatabase{corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}}}
	firstAlerts := &pollerAlerts{}
	first, err := NewWithConfig(
		database,
		&pollerTokens{},
		&pollerESI{streams: map[string][]esi.Notification{"1": {item}}},
		firstAlerts,
		Config{StateStore: stateStore},
	)
	if err != nil {
		t.Fatalf("new first poller: %v", err)
	}
	if err := first.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}

	secondAlerts := &pollerAlerts{}
	second, err := NewWithConfig(
		database,
		&pollerTokens{},
		&pollerESI{streams: map[string][]esi.Notification{"1": {item}}},
		secondAlerts,
		Config{StateStore: stateStore},
	)
	if err != nil {
		t.Fatalf("new second poller: %v", err)
	}
	if err := second.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if firstAlerts.count != 1 || secondAlerts.count != 0 {
		t.Fatalf("expected durable deduplication across poller instances, got first=%d second=%d", firstAlerts.count, secondAlerts.count)
	}
}

func TestProcessNotificationsDiscardsExistingStreamRowsOutsideLookbehind(t *testing.T) {
	now := time.Now().UTC()
	stateStore := notificationstate.NewMemoryStore()
	if err := stateStore.Save(context.Background(), "100", notificationstate.Cursor{
		Streams: map[string]notificationstate.Stream{
			"1": {LastTimestamp: now.Add(-30 * time.Minute)},
		},
	}); err != nil {
		t.Fatalf("save cursor: %v", err)
	}

	alertSink := &pollerAlerts{}
	service, err := NewWithConfig(
		&pollerDatabase{corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}}},
		&pollerTokens{},
		&pollerESI{},
		alertSink,
		Config{StateStore: stateStore, Lookbehind: 10 * time.Minute},
	)
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	corporation := authnextdb.Corporation{ID: "100", Name: "Corp", Ticker: "CORP"}
	item := esi.Notification{
		ID:        300,
		Type:      "StructureUnderAttack",
		Timestamp: now.Add(-20 * time.Minute),
		Text:      "structure_id: 123456789",
	}

	if err := service.processNotifications(context.Background(), corporation, "1", []esi.Notification{item}); err != nil {
		t.Fatalf("first process: %v", err)
	}
	if err := service.processNotifications(context.Background(), corporation, "1", []esi.Notification{item}); err != nil {
		t.Fatalf("second process: %v", err)
	}
	if alertSink.count != 0 {
		t.Fatalf("stale notification was delivered %d times", alertSink.count)
	}

	cursor, err := stateStore.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if got := cursor.Streams["1"].LastTimestamp; !got.Equal(item.Timestamp) {
		t.Fatalf("cursor timestamp = %v, want %v", got, item.Timestamp)
	}
}

func TestPollOncePersistsAndDrainsNotificationRetry(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 200, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	stateStore := notificationstate.NewMemoryStore()
	alertSink := &pollerAlerts{failures: 1}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, &pollerESI{streams: map[string][]esi.Notification{"1": {item}}}, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err == nil {
		t.Fatal("expected initial delivery failure")
	}
	pending, err := stateStore.ListPending(context.Background(), 10, time.Now().UTC().Add(time.Minute))
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected one pending retry, pending=%v err=%v", pending, err)
	}
	pending[0].NextRetryAt = time.Now().UTC().Add(-time.Second)
	if err := stateStore.ReschedulePending(context.Background(), &pending[0]); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("drain retry: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll after retry: %v", err)
	}
	if alertSink.count != 2 {
		t.Fatalf("expected one initial attempt and one retry, got %d", alertSink.count)
	}
	pending, err = stateStore.ListPending(context.Background(), 10, time.Now().UTC().Add(time.Minute))
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected retry to be removed after success, pending=%v err=%v", pending, err)
	}
}
