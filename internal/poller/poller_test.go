package poller

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/enrichment"
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
	count atomic.Int64
	store notificationstate.Store
}

func (*pollerAlerts) PreRoute(*enrichment.Envelope) []string {
	return []string{"test-destination"}
}

func (a *pollerAlerts) Enqueue(ctx context.Context, request *delivery.EnqueueRequest) (bool, error) {
	if request == nil {
		return false, errors.New("enqueue request is required")
	}
	if a.store == nil {
		a.count.Add(1)
		return true, nil
	}
	claimed, err := a.store.Admit(ctx, &notificationstate.Admission{
		NotificationID: request.Envelope.Event.NotificationID,
		CorporationID:  request.Envelope.Corporation.ID,
		Cursor:         request.Cursor,
		Pending: &notificationstate.PendingNotification{
			NotificationID:   request.Envelope.Event.NotificationID,
			CorporationID:    request.Envelope.Corporation.ID,
			CharacterID:      request.Envelope.CharacterID,
			NotificationJSON: request.Envelope.RawNotificationJSON,
			DestinationIDs:   request.DestinationIDs,
			CreatedAt:        time.Now().UTC(),
			NextRetryAt:      time.Now().UTC(),
		},
	})
	if err != nil {
		return false, err
	}
	if claimed {
		a.count.Add(1)
	}
	return claimed, nil
}

type failingSaveStateStore struct {
	*notificationstate.MemoryStore

	err error
}

func (s *failingSaveStateStore) Save(context.Context, string, notificationstate.Cursor) error {
	return s.err
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
	if alertSink.count.Load() != 0 {
		t.Fatalf("future notification deliveries = %d, want 0", alertSink.count.Load())
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

func TestPollOnceUsesTokenAndSuppressesDuplicate(t *testing.T) {
	esiClient := &pollerESI{timestamp: time.Now().UTC()}
	stateStore := notificationstate.NewMemoryStore()
	alertSink := &pollerAlerts{store: stateStore}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if alertSink.count.Load() != 1 {
		t.Fatalf("expected one delivery, got %d", alertSink.count.Load())
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
	if alertSink.count.Load() != 2 {
		t.Fatalf("expected both character streams to deliver, got %d", alertSink.count.Load())
	}
}

func TestPollOnceDeduplicatesGlobalNotificationIDAcrossCharacters(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 99, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	esiClient := &pollerESI{streams: map[string][]esi.Notification{"1": {item}, "2": {item}}}
	stateStore := notificationstate.NewMemoryStore()
	alertSink := &pollerAlerts{store: stateStore}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}},
	}, &pollerTokens{}, esiClient, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if alertSink.count.Load() != 1 {
		t.Fatalf("expected one delivery for globally unique notification ID, got %d", alertSink.count.Load())
	}
}

func TestPollOnceDeduplicatesNotificationIDAcrossCorporations(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 101, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	esiClient := &pollerESI{streams: map[string][]esi.Notification{"1": {item}, "2": {item}}}
	stateStore := notificationstate.NewMemoryStore()
	alertSink := &pollerAlerts{store: stateStore}
	service, err := NewWithConfig(&pollerDatabase{
		corporations: []authnextdb.Corporation{
			{ID: "100", Name: "Corp A", Ticker: "A"},
			{ID: "200", Name: "Corp B", Ticker: "B"},
		},
	}, &pollerTokens{}, esiClient, alertSink, Config{StateStore: stateStore})
	if err != nil {
		t.Fatalf("new poller: %v", err)
	}
	if err := service.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if alertSink.count.Load() != 1 {
		t.Fatalf("expected one delivery for globally unique notification ID, got %d", alertSink.count.Load())
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
	if alertSink.count.Load() != 0 {
		t.Fatalf("malformed notification was delivered %d times", alertSink.count.Load())
	}
}

func TestPollOnceRestoresCursorAcrossPollerInstances(t *testing.T) {
	now := time.Now().UTC()
	item := esi.Notification{ID: 100, Type: "StructureUnderAttack", Timestamp: now, Text: "structure_id: 123456789"}
	stateStore := notificationstate.NewMemoryStore()
	database := &pollerDatabase{corporations: []authnextdb.Corporation{{ID: "100", Name: "Corp", Ticker: "CORP"}}}
	firstAlerts := &pollerAlerts{store: stateStore}
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

	secondAlerts := &pollerAlerts{store: stateStore}
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
	if firstAlerts.count.Load() != 1 || secondAlerts.count.Load() != 0 {
		t.Fatalf("expected durable deduplication across poller instances, got first=%d second=%d", firstAlerts.count.Load(), secondAlerts.count.Load())
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
	if alertSink.count.Load() != 0 {
		t.Fatalf("stale notification was delivered %d times", alertSink.count.Load())
	}

	cursor, err := stateStore.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if got := cursor.Streams["1"].LastTimestamp; !got.Equal(item.Timestamp) {
		t.Fatalf("cursor timestamp = %v, want %v", got, item.Timestamp)
	}
}
