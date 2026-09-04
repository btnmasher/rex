// Package poller schedules per-corporation ESI notification polling.
package poller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/delivery"
	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/token"
	"github.com/btnmasher/rex/jobruntime/scheduler"
)

const (
	defaultConcurrency = 8
	defaultInterval    = time.Minute
	defaultLookbehind  = 10 * time.Minute
	maxLookbehind      = 10 * time.Minute
	seenRetention      = time.Hour
)

// TokenSource supplies corporation-scoped, refreshable EVE credentials.
type TokenSource interface {
	HasConfiguredTokens(string) bool
	NextAccessToken(context.Context, string) (token.Credential, error)
	RefreshCorporation(context.Context, string) error
	RefreshAccessToken(context.Context, string, string) (token.Credential, error)
}

// ESIClient is the notification endpoint required by the poller.
type ESIClient interface {
	Notifications(context.Context, string, string) ([]esi.Notification, error)
}

// AlertAdmission admits classified notifications for asynchronous delivery.
type AlertAdmission interface {
	delivery.Enqueuer
	PreRouter
}

// PreRouter selects candidate destinations using only raw notification data.
type PreRouter interface {
	PreRoute(*enrichment.Envelope) []string
}

// Database is the persistence subset needed by the polling scheduler.
type Database interface {
	ListNotificationCorporations(context.Context) ([]authnextdb.Corporation, error)
}

// Config controls polling cadence, concurrency, persistence, and filtering.
type Config struct {
	Interval    time.Duration
	Lookbehind  time.Duration
	Concurrency int
	Logger      *slog.Logger
	StateStore  notificationstate.Store
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = defaultInterval
	}
	if c.Lookbehind <= 0 {
		c.Lookbehind = defaultLookbehind
	}
	if c.Lookbehind > maxLookbehind {
		c.Lookbehind = maxLookbehind
	}
	if c.Concurrency <= 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.StateStore == nil {
		c.StateStore = notificationstate.NewMemoryStore()
	}
	return c
}

type cursor struct {
	lastTimestamp time.Time
	seenAtCursor  map[int64]struct{}
}

type cursorSnapshot struct {
	lastTimestamp time.Time
	seenAtCursor  map[int64]struct{}
}

type corporationState struct {
	streams map[string]*cursor
	loaded  bool
}

// Poller owns scheduler state and serializes polling per corporation.
type Poller struct {
	database    Database
	tokens      TokenSource
	esi         ESIClient
	admission   AlertAdmission
	interval    time.Duration
	lookbehind  time.Duration
	concurrency int
	logger      *slog.Logger
	stateStore  notificationstate.Store
	mu          sync.Mutex
	states      map[string]*corporationState
	polling     bool
}

// New creates a notification poller with default configuration.
func New(database Database, tokens TokenSource, esiClient ESIClient, admission AlertAdmission) (*Poller, error) {
	return NewWithConfig(database, tokens, esiClient, admission, Config{})
}

// NewWithConfig creates a notification poller with explicit runtime configuration.
func NewWithConfig(database Database, tokens TokenSource, esiClient ESIClient, admission AlertAdmission, config Config) (*Poller, error) {
	if database == nil || tokens == nil || esiClient == nil || admission == nil {
		return nil, errors.New("poller dependencies are required")
	}
	config = config.withDefaults()
	return &Poller{
		database:    database,
		tokens:      tokens,
		esi:         esiClient,
		admission:   admission,
		interval:    config.Interval,
		lookbehind:  config.Lookbehind,
		concurrency: config.Concurrency,
		logger:      config.Logger,
		stateStore:  config.StateStore,
		states:      make(map[string]*corporationState),
	}, nil
}

// Run polls immediately and then on wall-clock-aligned boundaries until cancellation.
func (p *Poller) Run(ctx context.Context) error {
	if p == nil {
		return errors.New("notification poller is unavailable")
	}
	if ctx == nil {
		return errors.New("notification poller context is required")
	}
	if err := p.PollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		p.logger.Error("initial notification poll failed", "err", err)
	}
	return scheduler.Run(ctx, p.interval, func(ctx context.Context) {
		if err := p.PollOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			p.logger.Error("notification poll failed", "err", err)
		}
	})
}

// PollOnce runs one bounded, non-overlapping poll for all eligible corporations.
func (p *Poller) PollOnce(ctx context.Context) (err error) {
	if p == nil {
		return errors.New("notification poller is unavailable")
	}
	if ctx == nil {
		return errors.New("notification poller context is required")
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			p.logger.Error("notification poll panic recovered",
				"panic_type", fmt.Sprintf("%T", recovered),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("notification poll panic: %v", recovered)
		}
	}()
	return p.pollOnce(ctx)
}

func (p *Poller) pollOnce(ctx context.Context) error {
	startedAt := time.Now()
	p.logger.Debug("notification poll started")
	p.mu.Lock()
	if p.polling {
		p.mu.Unlock()
		return nil
	}
	p.polling = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.polling = false
		p.mu.Unlock()
	}()
	if err := p.stateStore.PruneSeen(ctx, time.Now().UTC().Add(-seenRetention)); err != nil {
		return fmt.Errorf("prune seen notifications: %w", err)
	}
	corporations, err := p.database.ListNotificationCorporations(ctx)
	if err != nil {
		return err
	}
	pollingCorporations := p.queueableCorporations(corporations)
	p.logger.Debug("notification corporations loaded",
		"corporation_count", len(corporations),
		"queued_corporation_count", len(pollingCorporations),
	)
	pollErrors := p.pollCorporations(ctx, pollingCorporations)
	for _, err := range pollErrors {
		p.logCorporationPollFailure(err)
	}
	p.logger.Debug("notification poll completed",
		"corporation_count", len(corporations),
		"queued_corporation_count", len(pollingCorporations),
		"error_count", len(pollErrors),
		"duration", time.Since(startedAt),
	)
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(pollErrors...)
}

func (p *Poller) pollCorporations(ctx context.Context, corporations []authnextdb.Corporation) []error {
	semaphore := make(chan struct{}, p.concurrency)
	var wait sync.WaitGroup
	errs := make(chan error, len(corporations))
	for i := range corporations {
		corporation := corporations[i]
		wait.Go(func() {
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			if err := p.pollCorporationSafely(ctx, corporation); err != nil {
				errs <- err
			}
		})
	}
	wait.Wait()
	close(errs)
	pollErrors := make([]error, 0, len(errs))
	for err := range errs {
		pollErrors = append(pollErrors, err)
	}
	return pollErrors
}

func (p *Poller) queueableCorporations(corporations []authnextdb.Corporation) []authnextdb.Corporation {
	queued := make([]authnextdb.Corporation, 0, len(corporations))
	for _, corporation := range corporations {
		if !p.tokens.HasConfiguredTokens(corporation.ID) {
			p.logger.Debug("notification poll skipped",
				"corporation_id", corporation.ID,
				"reason", "no_configured_tokens",
			)
			continue
		}
		queued = append(queued, corporation)
	}
	return queued
}

func (p *Poller) logCorporationPollFailure(err error) {
	noTokenErr, ok := errors.AsType[*token.NoUsableTokenError](err)
	if !ok {
		p.logger.Warn("corporation notification poll failed", "err", err)
		return
	}
	p.logger.Warn("corporation notification poll failed",
		"corporation_id", noTokenErr.CorporationID,
		"reason", noTokenErr.Reason,
		"token_count", noTokenErr.TokenCount,
		"empty_token_count", noTokenErr.EmptyCount,
		"expired_token_count", noTokenErr.ExpiredCount,
		"err", err,
	)
}

func (p *Poller) pollCorporation(ctx context.Context, corporation authnextdb.Corporation) error {
	credential, err := p.nextAccessToken(ctx, corporation.ID)
	if err != nil {
		if errors.Is(err, token.ErrNotificationCooldown) {
			p.logger.Debug("notification poll skipped", cooldownLogAttrs(corporation.ID, err)...)
			return nil
		}
		return err
	}
	startedAt := time.Now()
	p.logger.Debug("polling ESI notifications",
		"corporation_id", corporation.ID,
		"character_id", credential.CharacterID,
	)
	items, err := p.esi.Notifications(ctx, credential.CharacterID, credential.AccessToken)
	if err != nil && isAuthenticationFailure(err) {
		p.logger.Debug("ESI notification authentication failed; refreshing access token",
			"corporation_id", corporation.ID,
			"character_id", credential.CharacterID,
		)
		credential, err = p.tokens.RefreshAccessToken(ctx, corporation.ID, credential.CharacterID)
		if err != nil {
			return err
		}
		items, err = p.esi.Notifications(ctx, credential.CharacterID, credential.AccessToken)
	}
	if err != nil {
		return err
	}
	p.logger.Debug("ESI notifications fetched",
		"corporation_id", corporation.ID,
		"character_id", credential.CharacterID,
		"notification_count", len(items),
		"duration", time.Since(startedAt),
	)
	return p.processNotifications(ctx, corporation, credential.CharacterID, items)
}

func (p *Poller) pollCorporationSafely(ctx context.Context, corporation authnextdb.Corporation) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			p.logger.Error("corporation notification poll panic recovered",
				"corporation_id", corporation.ID,
				"panic_type", fmt.Sprintf("%T", recovered),
				"panic", recovered,
				"stack", string(debug.Stack()),
			)
			err = fmt.Errorf("corporation %s notification poll panic: %v", corporation.ID, recovered)
		}
	}()
	return p.pollCorporation(ctx, corporation)
}

func (p *Poller) nextAccessToken(ctx context.Context, corporationID string) (token.Credential, error) {
	credential, err := p.tokens.NextAccessToken(ctx, corporationID)
	if err == nil || errors.Is(err, token.ErrNotificationCooldown) {
		return credential, err
	}
	var noTokenErr *token.NoUsableTokenError
	if !errors.As(err, &noTokenErr) {
		return token.Credential{}, err
	}

	p.logger.Debug("EVE token pool unusable; refreshing corporation tokens",
		"corporation_id", corporationID,
		"reason", noTokenErr.Reason,
		"token_count", noTokenErr.TokenCount,
		"empty_token_count", noTokenErr.EmptyCount,
		"expired_token_count", noTokenErr.ExpiredCount,
	)
	if refreshErr := p.tokens.RefreshCorporation(ctx, corporationID); refreshErr != nil {
		return token.Credential{}, errors.Join(err, fmt.Errorf("refresh EVE tokens for corporation %s: %w", corporationID, refreshErr))
	}
	p.logger.Debug("EVE token pool refreshed after selection failure", "corporation_id", corporationID)
	return p.tokens.NextAccessToken(ctx, corporationID)
}

func cooldownLogAttrs(corporationID string, err error) []any {
	cooldownErr, ok := errors.AsType[*token.CooldownError](err)
	if !ok {
		return []any{"corporation_id", corporationID, "reason", "token_cooldown"}
	}
	return []any{
		"corporation_id", corporationID,
		"reason", cooldownErr.Reason,
		"retry_after", cooldownErr.RetryAfter,
	}
}

func isAuthenticationFailure(err error) bool {
	var esiErr *esi.Error
	return errors.As(err, &esiErr) && esiErr.StatusCode == http.StatusUnauthorized
}

func (p *Poller) processNotifications(
	ctx context.Context,
	corporation authnextdb.Corporation,
	characterID string,
	items []esi.Notification,
) error {
	now := time.Now().UTC()
	futureIDs := futureNotificationIDs(items, now)
	items = normalizeNotificationTimestamps(items, now)

	state, err := p.streamSnapshot(ctx, corporation.ID, characterID)
	if err != nil {
		return err
	}
	cutoff := now.Add(-p.lookbehind)
	staleCount, err := p.markStaleNotifications(ctx, corporation.ID, characterID, items, state, cutoff, futureIDs)
	if err != nil {
		return err
	}
	eligible := eligibleNotifications(items, state, cutoff, futureIDs)
	p.logger.Debug("notifications evaluated",
		"corporation_id", corporation.ID,
		"character_id", characterID,
		"fetched_count", len(items),
		"stale_count", staleCount,
		"eligible_count", len(eligible),
	)
	return p.processEligibleNotifications(ctx, corporation, characterID, eligible)
}

func (p *Poller) markStaleNotifications(
	ctx context.Context,
	corporationID, characterID string,
	items []esi.Notification,
	state cursorSnapshot,
	cutoff time.Time,
	futureIDs map[int64]struct{},
) (int, error) {
	ordered := append([]esi.Notification(nil), items...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Timestamp.Equal(ordered[j].Timestamp) {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Timestamp.Before(ordered[j].Timestamp)
	})

	stale := make([]esi.Notification, 0, len(ordered))
	for i := range ordered {
		item := &ordered[i]
		if !isStaleNotification(item, state, cutoff, futureIDs) {
			continue
		}
		if item.ID > 0 {
			if _, err := p.stateStore.MarkSeen(ctx, item.ID); err != nil {
				return 0, fmt.Errorf("mark stale notification %d seen: %w", item.ID, err)
			}
		}
		stale = append(stale, *item)
	}
	if len(stale) == 0 {
		return 0, nil
	}
	if err := p.advanceStreams(ctx, corporationID, characterID, stale); err != nil {
		return 0, err
	}
	p.logger.Debug("stale notifications discarded",
		"corporation_id", corporationID,
		"character_id", characterID,
		"count", len(stale),
		"cutoff", cutoff,
	)
	return len(stale), nil
}

func (p *Poller) processEligibleNotifications(
	ctx context.Context,
	corporation authnextdb.Corporation,
	characterID string,
	eligible []esi.Notification,
) error {
	for i := range eligible {
		item := &eligible[i]
		if err := p.processEligibleNotification(ctx, corporation, characterID, item); err != nil {
			return err
		}
	}
	return nil
}

func (p *Poller) processEligibleNotification(
	ctx context.Context,
	corporation authnextdb.Corporation,
	characterID string,
	item *esi.Notification,
) error {
	if err := item.Validate(); err != nil {
		return p.dropMalformedNotification(ctx, corporation.ID, characterID, item, err)
	}
	event, ok := notifications.Classify(item)
	if !ok {
		p.logger.Debug("notification ignored",
			"notification_id", item.ID,
			"notification_type", item.Type,
			"reason", "unclassified",
		)
		_, err := p.stateStore.MarkSeen(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("mark notification %d seen: %w", item.ID, err)
		}
		return p.advanceStream(ctx, corporation.ID, characterID, item)
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode notification %d for retry: %w", item.ID, err)
	}
	pending := &notificationstate.PendingNotification{
		NotificationID:    item.ID,
		CorporationID:     corporation.ID,
		CorporationName:   corporation.Name,
		CorporationTicker: corporation.Ticker,
		CharacterID:       characterID,
		NotificationJSON:  append([]byte(nil), payload...),
	}
	envelope := &enrichment.Envelope{
		Corporation:         corporation,
		CharacterID:         characterID,
		RawNotificationJSON: payload,
		Event:               event,
	}
	destinationIDs := p.admission.PreRoute(envelope)
	pending.DestinationIDs = destinationIDs
	if len(destinationIDs) == 0 {
		pending = nil
	}
	claimed, err := p.admitNotification(ctx, envelope, item, pending)
	if err != nil {
		return fmt.Errorf("admit notification %d: %w", item.ID, err)
	}
	if pending == nil {
		p.logger.Debug("notification ignored",
			"notification_id", item.ID,
			"notification_type", item.Type,
			"reason", "no_configured_destinations",
		)
		return nil
	}
	if !claimed {
		p.logger.Debug("notification suppressed",
			"notification_id", item.ID,
			"reason", "already_seen_or_pending",
		)
		return nil
	}
	p.logger.Info("notification queued for alert delivery",
		"notification_id", item.ID,
		"alert_type", event.AlertType,
		"corporation_id", corporation.ID,
	)
	return nil
}

func (p *Poller) admitNotification(
	ctx context.Context,
	envelope *enrichment.Envelope,
	item *esi.Notification,
	pending *notificationstate.PendingNotification,
) (bool, error) {
	if envelope == nil || item == nil {
		return false, errors.New("notification admission input is required")
	}
	corporationID := envelope.Corporation.ID
	if err := p.loadState(ctx, corporationID); err != nil {
		return false, err
	}
	p.mu.Lock()
	state := p.stateLocked(corporationID)
	candidate := p.cursorLocked(state)
	applyCursorItems(&candidate, envelope.CharacterID, []esi.Notification{*item}, time.Now().UTC())
	p.mu.Unlock()

	claimed, err := p.admission.Enqueue(ctx, &delivery.EnqueueRequest{
		Envelope:       *envelope,
		DestinationIDs: pendingDestinationIDs(pending),
		Cursor:         candidate,
	})
	if err != nil {
		return false, err
	}
	p.mu.Lock()
	state = p.stateLocked(corporationID)
	stream := candidate.Streams[envelope.CharacterID]
	state.streams[envelope.CharacterID] = &cursor{
		lastTimestamp: stream.LastTimestamp,
		seenAtCursor:  stream.SeenAtCursor,
	}
	p.mu.Unlock()
	return claimed, nil
}

func pendingDestinationIDs(pending *notificationstate.PendingNotification) []string {
	if pending == nil {
		return nil
	}
	return append([]string(nil), pending.DestinationIDs...)
}

func applyCursorItems(cursor *notificationstate.Cursor, characterID string, items []esi.Notification, now time.Time) {
	if cursor == nil {
		return
	}
	stream := cursor.Streams[characterID]
	if stream.SeenAtCursor == nil {
		stream.SeenAtCursor = make(map[int64]struct{})
	}
	for index := range items {
		item := &items[index]
		cursorTimestamp := item.Timestamp
		if cursorTimestamp.After(now) {
			cursorTimestamp = now
		}
		if cursorTimestamp.After(stream.LastTimestamp) {
			stream.LastTimestamp = cursorTimestamp
			stream.SeenAtCursor = make(map[int64]struct{})
		}
		if cursorTimestamp.Equal(stream.LastTimestamp) {
			stream.SeenAtCursor[item.ID] = struct{}{}
		}
	}
	cursor.Streams[characterID] = stream
}

func (p *Poller) dropMalformedNotification(
	ctx context.Context,
	corporationID, characterID string,
	item *esi.Notification,
	validationErr error,
) error {
	p.logger.Warn("malformed ESI notification dropped",
		"corporation_id", corporationID,
		"character_id", characterID,
		"notification_id", item.ID,
		"notification_type", item.Type,
		"reason", validationErr,
	)
	if item.ID > 0 {
		if _, err := p.stateStore.MarkSeen(ctx, item.ID); err != nil {
			return fmt.Errorf("mark malformed notification %d seen: %w", item.ID, err)
		}
	}
	return p.advanceStream(ctx, corporationID, characterID, item)
}

func eligibleNotifications(items []esi.Notification, state cursorSnapshot, cutoff time.Time, futureIDs map[int64]struct{}) []esi.Notification {
	eligible := make([]esi.Notification, 0, len(items))
	for _, item := range items {
		if isStaleNotification(&item, state, cutoff, futureIDs) {
			continue
		}
		eligible = append(eligible, item)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].Timestamp.Equal(eligible[j].Timestamp) {
			return eligible[i].ID < eligible[j].ID
		}
		return eligible[i].Timestamp.Before(eligible[j].Timestamp)
	})
	return eligible
}

func normalizeNotificationTimestamps(items []esi.Notification, now time.Time) []esi.Notification {
	for i := range items {
		if items[i].Timestamp.After(now) {
			items[i].Timestamp = now
		}
	}
	return items
}

func futureNotificationIDs(items []esi.Notification, now time.Time) map[int64]struct{} {
	futureIDs := make(map[int64]struct{})
	for i := range items {
		if items[i].ID > 0 && items[i].Timestamp.After(now) {
			futureIDs[items[i].ID] = struct{}{}
		}
	}
	return futureIDs
}

func isStaleNotification(item *esi.Notification, state cursorSnapshot, cutoff time.Time, futureIDs map[int64]struct{}) bool {
	if _, ok := futureIDs[item.ID]; ok {
		return true
	}
	if item.Timestamp.Before(cutoff) {
		return true
	}
	if item.Timestamp.Before(state.lastTimestamp) {
		return true
	}
	return item.Timestamp.Equal(state.lastTimestamp) && hasSeen(state.seenAtCursor, item.ID)
}

func hasSeen(seen map[int64]struct{}, notificationID int64) bool {
	_, ok := seen[notificationID]
	return ok
}

func (p *Poller) streamSnapshot(ctx context.Context, corporationID, characterID string) (cursorSnapshot, error) {
	if err := p.loadState(ctx, corporationID); err != nil {
		return cursorSnapshot{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.stateLocked(corporationID)
	stream, ok := state.streams[characterID]
	if !ok {
		stream = &cursor{lastTimestamp: time.Now().UTC().Add(-p.lookbehind), seenAtCursor: make(map[int64]struct{})}
		state.streams[characterID] = stream
	}
	seenAtCursor := make(map[int64]struct{}, len(stream.seenAtCursor))
	for notificationID := range stream.seenAtCursor {
		seenAtCursor[notificationID] = struct{}{}
	}
	return cursorSnapshot{lastTimestamp: stream.lastTimestamp, seenAtCursor: seenAtCursor}, nil
}

func (p *Poller) advanceStream(ctx context.Context, corporationID, characterID string, item *esi.Notification) error {
	return p.advanceStreams(ctx, corporationID, characterID, []esi.Notification{*item})
}

func (p *Poller) advanceStreams(ctx context.Context, corporationID, characterID string, items []esi.Notification) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	state := p.stateLocked(corporationID)
	stream := state.streams[characterID]
	if stream == nil {
		return fmt.Errorf("notification stream %s for corporation %s is not initialized", characterID, corporationID)
	}
	candidate := p.cursorLocked(state)
	candidateStream := candidate.Streams[characterID]
	if candidateStream.SeenAtCursor == nil {
		candidateStream.SeenAtCursor = make(map[int64]struct{})
	}
	now := time.Now().UTC()
	for i := range items {
		item := &items[i]
		// Prevent malformed or clock-skewed upstream timestamps from poisoning the cursor.
		cursorTimestamp := item.Timestamp
		if cursorTimestamp.After(now) {
			cursorTimestamp = now
		}
		if cursorTimestamp.After(candidateStream.LastTimestamp) {
			candidateStream.LastTimestamp = cursorTimestamp
			candidateStream.SeenAtCursor = make(map[int64]struct{})
		}
		if cursorTimestamp.Equal(candidateStream.LastTimestamp) {
			candidateStream.SeenAtCursor[item.ID] = struct{}{}
		}
	}
	candidate.Streams[characterID] = candidateStream
	if err := p.stateStore.Save(ctx, corporationID, candidate); err != nil {
		return err
	}
	state.streams[characterID] = &cursor{
		lastTimestamp: candidateStream.LastTimestamp,
		seenAtCursor:  candidateStream.SeenAtCursor,
	}
	return nil
}

func (p *Poller) stateLocked(corporationID string) *corporationState {
	state := p.states[corporationID]
	if state == nil {
		state = &corporationState{streams: make(map[string]*cursor)}
		p.states[corporationID] = state
	}
	return state
}

func (p *Poller) loadState(ctx context.Context, corporationID string) error {
	p.mu.Lock()
	state := p.stateLocked(corporationID)
	if state.loaded {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	persisted, err := p.stateStore.Load(ctx, corporationID)
	if err != nil {
		return fmt.Errorf("load notification state for corporation %s: %w", corporationID, err)
	}
	p.mu.Lock()
	state = p.stateLocked(corporationID)
	if !state.loaded {
		for characterID, stream := range persisted.Streams {
			seen := make(map[int64]struct{}, len(stream.SeenAtCursor))
			for notificationID := range stream.SeenAtCursor {
				seen[notificationID] = struct{}{}
			}
			state.streams[characterID] = &cursor{lastTimestamp: stream.LastTimestamp, seenAtCursor: seen}
		}
		state.loaded = true
	}
	p.mu.Unlock()
	return nil
}

func (p *Poller) cursorLocked(state *corporationState) notificationstate.Cursor {
	cursor := notificationstate.Cursor{Streams: make(map[string]notificationstate.Stream, len(state.streams))}
	for characterID, stream := range state.streams {
		if stream == nil {
			continue
		}
		seen := make(map[int64]struct{}, len(stream.seenAtCursor))
		for notificationID := range stream.seenAtCursor {
			seen[notificationID] = struct{}{}
		}
		cursor.Streams[characterID] = notificationstate.Stream{LastTimestamp: stream.lastTimestamp, SeenAtCursor: seen}
	}
	return cursor
}
