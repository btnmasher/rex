// Package delivery durably queues and dispatches enriched notifications.
package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
)

const (
	defaultConcurrency  = 4
	queueBatchSize      = 32
	defaultWakeInterval = time.Second
	maxAttempts         = 6
	maxRetryAge         = 30 * time.Minute
	maxRetryDelay       = 30 * time.Minute
	maxErrorLength      = 4096
	initialRetryDelay   = 5 * time.Second
	maxBackoffDelay     = 5 * time.Minute
)

// EnqueueRequest contains one immutable alert envelope, its pre-route plan,
// and the cursor state that must be committed with admission.
type EnqueueRequest struct {
	Envelope       enrichment.Envelope
	DestinationIDs []string
	Cursor         notificationstate.Cursor
}

// Enqueuer durably admits notifications for asynchronous delivery.
type Enqueuer interface {
	Enqueue(context.Context, *EnqueueRequest) (bool, error)
}

// PostRouter removes candidate destinations using enriched notification data.
type PostRouter interface {
	PostRoute(*enrichment.Context, []string) []string
}

// OutcomeStatus identifies the provider-independent result of one target send.
type OutcomeStatus uint8

const (
	// OutcomeAccepted means the destination accepted the notification.
	OutcomeAccepted OutcomeStatus = iota + 1
	// OutcomeRetryable means the target should be retried after RetryAfter.
	OutcomeRetryable
	// OutcomePermanent means the target should not be retried.
	OutcomePermanent
)

// Outcome is the normalized result returned by a destination adapter.
type Outcome struct {
	Status     OutcomeStatus
	RetryAfter time.Duration
	Err        error
}

// Adapter presents and sends one enriched alert to one destination target.
type Adapter interface {
	Deliver(context.Context, *enrichment.Context, string) Outcome
}

// TerminalRecorder records targets that are dropped after delivery failure.
type TerminalRecorder interface {
	RecordTerminalFailure(context.Context, *enrichment.Context, []string, error) error
}

// Config controls the asynchronous delivery worker.
type Config struct {
	Store          notificationstate.Store
	Enricher       enrichment.Enricher
	Router         PostRouter
	Adapter        Adapter
	TerminalRecord TerminalRecorder
	Concurrency    int
	Logger         *slog.Logger
	Now            func() time.Time
}

// Worker owns asynchronous delivery retries independently from ESI polling.
type Worker struct {
	store          notificationstate.Store
	enricher       enrichment.Enricher
	router         PostRouter
	adapter        Adapter
	terminalRecord TerminalRecorder
	concurrency    int
	logger         *slog.Logger
	now            func() time.Time
	wake           chan struct{}
	slots          chan struct{}
	activeMu       sync.Mutex
	active         map[int64]struct{}
	workers        sync.WaitGroup
}

// New creates a bounded notification delivery worker.
func New(config *Config) (*Worker, error) {
	if config == nil {
		return nil, errors.New("delivery worker configuration is required")
	}
	if nilInterface(config.Store) || nilInterface(config.Enricher) || nilInterface(config.Router) || nilInterface(config.Adapter) {
		return nil, errors.New("delivery worker dependencies are required")
	}
	if config.Concurrency <= 0 {
		config.Concurrency = defaultConcurrency
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Worker{
		store:          config.Store,
		enricher:       config.Enricher,
		router:         config.Router,
		adapter:        config.Adapter,
		terminalRecord: config.TerminalRecord,
		concurrency:    config.Concurrency,
		logger:         config.Logger,
		now:            config.Now,
		wake:           make(chan struct{}, 1),
		slots:          make(chan struct{}, config.Concurrency),
		active:         make(map[int64]struct{}),
	}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// Enqueue atomically admits an alert and wakes the worker without waiting for
// enrichment, presentation, transport, or retry delays.
func (w *Worker) Enqueue(ctx context.Context, request *EnqueueRequest) (bool, error) {
	if w == nil || w.store == nil {
		return false, errors.New("delivery worker is unavailable")
	}
	if ctx == nil {
		return false, errors.New("delivery enqueue context is required")
	}
	if request == nil {
		return false, errors.New("delivery enqueue request is required")
	}
	now := w.now().UTC()
	pending := &notificationstate.PendingNotification{
		NotificationID:    request.Envelope.Event.NotificationID,
		CorporationID:     request.Envelope.Corporation.ID,
		CorporationName:   request.Envelope.Corporation.Name,
		CorporationTicker: request.Envelope.Corporation.Ticker,
		CharacterID:       request.Envelope.CharacterID,
		NotificationJSON:  append([]byte(nil), request.Envelope.RawNotificationJSON...),
		DestinationIDs:    append([]string(nil), request.DestinationIDs...),
		CreatedAt:         now,
		NextRetryAt:       now,
	}
	if len(request.DestinationIDs) == 0 {
		pending = nil
	}
	claimed, err := w.store.Admit(ctx, &notificationstate.Admission{
		NotificationID: request.Envelope.Event.NotificationID,
		CorporationID:  request.Envelope.Corporation.ID,
		Cursor:         request.Cursor,
		Pending:        pending,
	})
	if err != nil {
		return false, err
	}
	if claimed {
		w.signal()
	}
	return claimed, nil
}

// Run drains due delivery rows until ctx is canceled.
func (w *Worker) Run(ctx context.Context) error {
	if w == nil {
		return errors.New("delivery worker is unavailable")
	}
	if ctx == nil {
		return errors.New("delivery worker context is required")
	}
	w.drain(ctx)
	timer := time.NewTimer(defaultWakeInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			w.workers.Wait()
			return ctx.Err()
		case <-w.wake:
			w.drain(ctx)
		case <-timer.C:
			w.drain(ctx)
			timer.Reset(defaultWakeInterval)
		}
	}
}

func (w *Worker) drain(ctx context.Context) {
	due, err := w.store.ListPending(ctx, queueBatchSize, w.now().UTC())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.logger.Warn("notification delivery queue drain failed", "err", err)
		}
		return
	}
	for index := range due {
		item := due[index]
		if !w.activate(item.NotificationID) {
			continue
		}
		select {
		case w.slots <- struct{}{}:
		case <-ctx.Done():
			w.deactivate(item.NotificationID)
			return
		}
		w.workers.Add(1)
		itemCopy := item
		go func() {
			defer w.workers.Done()
			defer func() { <-w.slots }()
			defer w.deactivate(itemCopy.NotificationID)
			w.processSafely(ctx, &itemCopy)
		}()
	}
}

func (w *Worker) processSafely(ctx context.Context, item *notificationstate.PendingNotification) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err := errors.New("notification delivery panic")
			w.logger.Error("notification delivery panic recovered",
				"notification_id", pendingID(item),
				"panic_type", fmt.Sprintf("%T", recovered),
				"stack", string(debug.Stack()),
			)
			w.rescheduleAfterPanic(ctx, item, err)
		}
	}()
	w.process(ctx, item)
}

func (w *Worker) rescheduleAfterPanic(ctx context.Context, item *notificationstate.PendingNotification, reason error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("notification delivery panic recovery failed",
				"notification_id", pendingID(item),
				"panic_type", fmt.Sprintf("%T", recovered),
				"stack", string(debug.Stack()),
			)
		}
	}()
	w.reschedule(ctx, item, nil, 0, reason)
}

func (w *Worker) process(ctx context.Context, item *notificationstate.PendingNotification) {
	if item == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	if retryExpired(item, w.now().UTC()) {
		w.terminal(ctx, item, nil, pendingDestinationIDsForFailure(item), errors.New("notification delivery retry expired"))
		return
	}
	var eventEnvelope enrichment.Envelope
	if err := decodeEnvelope(item, &eventEnvelope); err != nil {
		w.drop(ctx, item, err)
		return
	}
	enriched, err := w.enricher.Enrich(ctx, &eventEnvelope)
	if err != nil {
		w.reschedule(ctx, item, nil, 0, err)
		return
	}
	candidateIDs := item.DestinationIDs
	if len(item.FailedDestinationIDs) > 0 {
		candidateIDs = item.FailedDestinationIDs
	}
	targets := w.router.PostRoute(enriched, undeliveredTargets(candidateIDs, item.DeliveredDestinationIDs))
	if len(targets) == 0 {
		w.delete(ctx, item)
		return
	}
	results := w.deliverTargets(ctx, item, enriched, targets)
	if results.aborted {
		return
	}
	if len(results.retryable) == 0 && len(results.permanent) == 0 {
		w.delete(ctx, item)
		return
	}
	if len(results.permanent) > 0 {
		w.recordTerminal(ctx, enriched, results.permanent, errors.Join(results.permanentErrors...))
	}
	if len(results.retryable) == 0 {
		w.delete(ctx, item)
		return
	}
	w.reschedule(ctx, item, results.retryable, results.retryAfter, errors.Join(results.retryableErrors...))
}

type deliveryResults struct {
	aborted         bool
	retryable       []string
	permanent       []string
	retryAfter      time.Duration
	retryableErrors []error
	permanentErrors []error
}

func (w *Worker) deliverTargets(ctx context.Context, item *notificationstate.PendingNotification, enriched *enrichment.Context, targets []string) deliveryResults {
	results := deliveryResults{
		retryable: make([]string, 0, len(targets)),
		permanent: make([]string, 0, len(targets)),
	}
	for _, targetID := range targets {
		if err := ctx.Err(); err != nil {
			results.aborted = true
			return results
		}
		outcome := w.deliverTargetSafely(ctx, enriched, targetID)
		switch outcome.Status {
		case OutcomeAccepted:
			if err := w.persistAcceptedTarget(ctx, item, targetID); err != nil {
				w.logger.Warn("notification delivery progress persistence failed",
					"notification_id", pendingID(item),
					"target_id", targetID,
					"err", err,
				)
				results.aborted = true
				return results
			}
		case OutcomeRetryable:
			results.retryable = append(results.retryable, targetID)
			results.retryableErrors = appendOutcomeError(results.retryableErrors, outcome.Err)
			results.retryAfter = max(results.retryAfter, outcome.RetryAfter)
		case OutcomePermanent:
			results.permanent = append(results.permanent, targetID)
			results.permanentErrors = appendOutcomeError(results.permanentErrors, outcome.Err)
		default:
			results.permanent = append(results.permanent, targetID)
			results.permanentErrors = append(results.permanentErrors, errors.New("destination adapter returned an invalid outcome"))
		}
	}
	return results
}

func (w *Worker) persistAcceptedTarget(ctx context.Context, item *notificationstate.PendingNotification, targetID string) error {
	if item == nil {
		return errors.New("pending notification is required")
	}
	if containsTarget(item.DeliveredDestinationIDs, targetID) {
		return nil
	}
	item.DeliveredDestinationIDs = append(item.DeliveredDestinationIDs, targetID)
	return w.store.ReschedulePending(ctx, item)
}

func undeliveredTargets(targets, delivered []string) []string {
	if len(targets) == 0 || len(delivered) == 0 {
		return append([]string(nil), targets...)
	}
	remaining := make([]string, 0, len(targets))
	for _, target := range targets {
		if !containsTarget(delivered, target) {
			remaining = append(remaining, target)
		}
	}
	return remaining
}

func containsTarget(targets []string, target string) bool {
	return slices.Contains(targets, target)
}

func (w *Worker) deliverTargetSafely(ctx context.Context, enriched *enrichment.Context, targetID string) (outcome Outcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			w.logger.Error("destination adapter panic recovered",
				"target_id", targetID,
				"panic_type", fmt.Sprintf("%T", recovered),
				"stack", string(debug.Stack()),
			)
			outcome = Outcome{Status: OutcomeRetryable, Err: errors.New("destination adapter panic")}
		}
	}()
	return w.adapter.Deliver(ctx, enriched, targetID)
}

func appendOutcomeError(errs []error, err error) []error {
	if err == nil {
		return errs
	}
	return append(errs, err)
}

func (w *Worker) reschedule(ctx context.Context, item *notificationstate.PendingNotification, failed []string, retryAfter time.Duration, reason error) {
	if item == nil {
		return
	}
	item.Attempts++
	if len(failed) > 0 {
		item.FailedDestinationIDs = append([]string(nil), failed...)
	} else {
		item.FailedDestinationIDs = append([]string(nil), item.DestinationIDs...)
	}
	if retryExpired(item, w.now().UTC()) || item.Attempts >= maxAttempts {
		w.terminal(ctx, item, nil, item.FailedDestinationIDs, reason)
		return
	}
	delay := retryDelay(item.Attempts)
	delay = max(delay, retryAfter)
	delay = min(delay, maxRetryDelay)
	item.NextRetryAt = w.now().UTC().Add(delay)
	item.LastError = safeError(reason)
	if err := w.store.ReschedulePending(ctx, item); err != nil {
		w.logger.Warn("notification delivery retry persistence failed", "notification_id", item.NotificationID, "err", err)
		return
	}
	w.logger.Warn("notification delivery retry scheduled",
		"notification_id", item.NotificationID,
		"attempts", item.Attempts,
		"next_retry_at", item.NextRetryAt,
		"failed_destination_ids", item.FailedDestinationIDs,
		"err", item.LastError,
	)
}

func (w *Worker) terminal(ctx context.Context, item *notificationstate.PendingNotification, enriched *enrichment.Context, failed []string, reason error) {
	if item == nil {
		return
	}
	if enriched == nil {
		enriched = minimalContext(item)
	}
	w.recordTerminal(ctx, enriched, failed, reason)
	w.delete(ctx, item)
	w.logger.Warn("notification delivery dropped after retry failure",
		"notification_id", item.NotificationID,
		"attempts", item.Attempts,
		"err", safeError(reason),
	)
}

func minimalContext(item *notificationstate.PendingNotification) *enrichment.Context {
	var envelope enrichment.Envelope
	if err := decodeEnvelope(item, &envelope); err != nil {
		return nil
	}
	return &enrichment.Context{
		CorporationID:            envelope.Corporation.ID,
		CorporationName:          envelope.Corporation.Name,
		CorporationTicker:        envelope.Corporation.Ticker,
		Event:                    envelope.Event,
		CharacterID:              envelope.CharacterID,
		RawNotificationJSON:      append([]byte(nil), envelope.RawNotificationJSON...),
		PollingCorporationID:     envelope.Corporation.ID,
		PollingCorporationName:   envelope.Corporation.Name,
		PollingCorporationTicker: envelope.Corporation.Ticker,
	}
}

func pendingDestinationIDsForFailure(item *notificationstate.PendingNotification) []string {
	if item == nil {
		return nil
	}
	if len(item.FailedDestinationIDs) > 0 {
		return append([]string(nil), item.FailedDestinationIDs...)
	}
	return append([]string(nil), item.DestinationIDs...)
}

func (w *Worker) recordTerminal(ctx context.Context, enriched *enrichment.Context, failed []string, reason error) {
	if w == nil || w.terminalRecord == nil || enriched == nil {
		return
	}
	if reason == nil {
		reason = errors.New("destination delivery failed")
	}
	if err := recordTerminalSafely(ctx, w.terminalRecord, enriched, failed, reason); err != nil {
		w.logger.Warn("terminal alert failure history record failed", "notification_id", enriched.Event.NotificationID, "err", err)
	}
}

func recordTerminalSafely(ctx context.Context, recorder TerminalRecorder, enriched *enrichment.Context, failed []string, reason error) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = errors.New("terminal alert history persistence panic")
		}
	}()
	return recorder.RecordTerminalFailure(ctx, enriched, failed, reason)
}

func (w *Worker) drop(ctx context.Context, item *notificationstate.PendingNotification, reason error) {
	w.delete(ctx, item)
	w.logger.Warn("malformed queued notification dropped", "notification_id", pendingID(item), "err", safeError(reason))
}

func (w *Worker) delete(ctx context.Context, item *notificationstate.PendingNotification) {
	if item == nil {
		return
	}
	if err := w.store.DeletePending(ctx, item.NotificationID); err != nil {
		w.logger.Warn("notification delivery queue cleanup failed", "notification_id", item.NotificationID, "err", err)
	}
}

func (w *Worker) activate(notificationID int64) bool {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if _, ok := w.active[notificationID]; ok {
		return false
	}
	w.active[notificationID] = struct{}{}
	return true
}

func (w *Worker) deactivate(notificationID int64) {
	w.activeMu.Lock()
	delete(w.active, notificationID)
	w.activeMu.Unlock()
}

func (w *Worker) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func retryExpired(item *notificationstate.PendingNotification, now time.Time) bool {
	return item == nil || !item.CreatedAt.IsZero() && !now.Before(item.CreatedAt.Add(maxRetryAge))
}

func retryDelay(attempt int) time.Duration {
	delay := initialRetryDelay
	for index := 1; index < attempt; index++ {
		if delay >= maxBackoffDelay/2 {
			return maxBackoffDelay
		}
		delay *= 2
	}
	return delay
}

func decodeEnvelope(item *notificationstate.PendingNotification, envelope *enrichment.Envelope) error {
	if item == nil || envelope == nil {
		return errors.New("queued notification is required")
	}
	var notification esi.Notification
	if err := json.Unmarshal(item.NotificationJSON, &notification); err != nil {
		return fmt.Errorf("decode queued notification: %w", err)
	}
	if err := notification.Validate(); err != nil {
		return fmt.Errorf("validate queued notification: %w", err)
	}
	event, ok := notifications.Classify(&notification)
	if !ok {
		return errors.New("queued notification is unclassified")
	}
	envelope.Event = event
	envelope.Corporation.ID = item.CorporationID
	envelope.Corporation.Name = item.CorporationName
	envelope.Corporation.Ticker = item.CorporationTicker
	envelope.CharacterID = item.CharacterID
	envelope.RawNotificationJSON = append([]byte(nil), item.NotificationJSON...)
	return nil
}

func pendingID(item *notificationstate.PendingNotification) int64 {
	if item == nil {
		return 0
	}
	return item.NotificationID
}

func safeError(err error) string {
	if err == nil {
		return "destination delivery failed"
	}
	message := err.Error()
	if len(message) > maxErrorLength {
		return message[:maxErrorLength]
	}
	return message
}
