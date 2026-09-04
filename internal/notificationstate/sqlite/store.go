// Package sqlite provides durable notification cursor storage.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/notificationstate/sqlite/gen"
	_ "modernc.org/sqlite"
)

const (
	maxStateBytes          = 2 << 20
	maxHistoryPayloadBytes = 512 << 10
	maxHistoryErrorBytes   = 4 << 10
	defaultPendingLimit    = 32
	defaultHistoryLimit    = 100
	alertHistoryRetention  = 30 * 24 * time.Hour
)

// Store persists notification cursors in a single-connection SQLite database.
type Store struct {
	db      *sql.DB
	queries *gen.Queries
}

var _ notificationstate.Store = (*Store)(nil)
var _ notificationstate.AlertHistory = (*Store)(nil)

// NewStore opens or creates a durable cursor database and applies Goose migrations using ctx.
func NewStore(ctx context.Context, path string) (*Store, error) {
	if ctx == nil {
		return nil, errors.New("notification state database context is required")
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("notification state SQLite path is required")
	}
	db, err := sql.Open("sqlite", withDefaults(path))
	if err != nil {
		return nil, fmt.Errorf("open notification state SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping notification state SQLite database: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate notification state schema: %w", err)
	}
	store := &Store{db: db, queries: gen.New(db)}
	if err := store.PruneAlertHistory(ctx, time.Now().UTC().Add(-alertHistoryRetention)); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("prune notification alert history: %w", err)
	}
	return store, nil
}

// Close closes the SQLite database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Load returns the persisted cursor or an empty cursor when none exists.
func (s *Store) Load(ctx context.Context, corporationID string) (notificationstate.Cursor, error) {
	if err := s.validateContext(ctx); err != nil {
		return notificationstate.Cursor{}, err
	}
	stateJSON, err := s.queries.GetCursor(ctx, corporationID)
	if errors.Is(err, sql.ErrNoRows) {
		return notificationstate.Cursor{}, nil
	}
	if err != nil {
		return notificationstate.Cursor{}, fmt.Errorf("load notification cursor: %w", err)
	}
	if len(stateJSON) > maxStateBytes {
		return notificationstate.Cursor{}, errors.New("notification cursor exceeds size limit")
	}
	var cursor notificationstate.Cursor
	if err := json.Unmarshal(stateJSON, &cursor); err != nil {
		return notificationstate.Cursor{}, fmt.Errorf("decode notification cursor: %w", err)
	}
	return cursor, nil
}

// Save replaces the persisted cursor for one corporation.
func (s *Store) Save(ctx context.Context, corporationID string, cursor notificationstate.Cursor) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("encode notification cursor: %w", err)
	}
	if len(encoded) > maxStateBytes {
		return errors.New("notification cursor exceeds size limit")
	}
	if err := s.queries.SaveCursor(ctx, gen.SaveCursorParams{CorporationID: corporationID, StateJson: encoded}); err != nil {
		return fmt.Errorf("save notification cursor: %w", err)
	}
	return nil
}

// MarkSeen atomically claims a globally unique notification ID.
func (s *Store) MarkSeen(ctx context.Context, notificationID int64) (bool, error) {
	if err := s.validateContext(ctx); err != nil {
		return false, err
	}
	result, err := s.queries.MarkSeen(ctx, notificationID)
	if err != nil {
		return false, fmt.Errorf("mark notification %d seen: %w", notificationID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect notification %d seen result: %w", notificationID, err)
	}
	return rows == 1, nil
}

// Admit atomically marks a notification seen, persists its cursor, and stores
// its pending delivery payload when one is supplied.
func (s *Store) Admit(ctx context.Context, admission *notificationstate.Admission) (bool, error) {
	if s == nil {
		return false, errors.New("notification store is unavailable")
	}
	if ctx == nil {
		return false, errors.New("notification admission context is required")
	}
	if err := admission.Validate(); err != nil {
		return false, err
	}
	cursorJSON, err := json.Marshal(admission.Cursor)
	if err != nil {
		return false, fmt.Errorf("encode notification cursor: %w", err)
	}
	if len(cursorJSON) > maxStateBytes {
		return false, errors.New("notification cursor exceeds size limit")
	}
	pendingData, err := encodePending(admission.Pending)
	if err != nil {
		return false, err
	}
	return s.admitTransaction(ctx, admission, cursorJSON, &pendingData)
}

type encodedPending struct {
	destinationIDs []byte
	deliveredIDs   []byte
	failedIDs      []byte
	createdAt      time.Time
}

func encodePending(pending *notificationstate.PendingNotification) (encodedPending, error) {
	if pending == nil {
		return encodedPending{}, nil
	}
	destinationIDs, err := json.Marshal(pending.DestinationIDs)
	if err != nil {
		return encodedPending{}, fmt.Errorf("encode pending notification destinations: %w", err)
	}
	deliveredIDs, err := json.Marshal(pending.DeliveredDestinationIDs)
	if err != nil {
		return encodedPending{}, fmt.Errorf("encode delivered notification destinations: %w", err)
	}
	failedIDs, err := json.Marshal(pending.FailedDestinationIDs)
	if err != nil {
		return encodedPending{}, fmt.Errorf("encode failed notification destinations: %w", err)
	}
	createdAt := pending.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	return encodedPending{destinationIDs: destinationIDs, deliveredIDs: deliveredIDs, failedIDs: failedIDs, createdAt: createdAt}, nil
}

func insertPending(ctx context.Context, queries *gen.Queries, pending *notificationstate.PendingNotification, data *encodedPending) error {
	if queries == nil || pending == nil {
		return errors.New("pending notification storage is unavailable")
	}
	if data == nil {
		return errors.New("pending notification encoding is required")
	}
	return queries.InsertPending(ctx, gen.InsertPendingParams{
		NotificationID:              pending.NotificationID,
		CorporationID:               pending.CorporationID,
		CorporationName:             pending.CorporationName,
		CorporationTicker:           pending.CorporationTicker,
		CharacterID:                 pending.CharacterID,
		NotificationJson:            pending.NotificationJSON,
		DestinationIdsJson:          data.destinationIDs,
		DeliveredDestinationIdsJson: data.deliveredIDs,
		FailedDestinationIdsJson:    data.failedIDs,
		Attempts:                    int64(pending.Attempts),
		CreatedAt:                   data.createdAt.Unix(),
		NextRetryAt:                 pending.NextRetryAt.Unix(),
		LastError:                   pending.LastError,
	})
}

// ListPending returns due retry payloads in deterministic order.
func (s *Store) ListPending(ctx context.Context, limit int, now time.Time) ([]notificationstate.PendingNotification, error) {
	if err := s.validateContext(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultPendingLimit
	}
	rows, err := s.queries.ListPending(ctx, gen.ListPendingParams{NextRetryAt: now.Unix(), Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list pending notifications: %w", err)
	}
	items := make([]notificationstate.PendingNotification, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		pending, err := decodePending(row)
		if err != nil {
			if err := s.dropMalformedPending(ctx, row.NotificationID, err); err != nil {
				return nil, err
			}
			continue
		}
		items = append(items, pending)
	}
	return items, nil
}

func decodePending(row *gen.ListPendingRow) (notificationstate.PendingNotification, error) {
	if row == nil {
		return notificationstate.PendingNotification{}, errors.New("pending notification row is required")
	}
	var destinationIDs, deliveredDestinations, failedDestinations []string
	if err := json.Unmarshal(row.DestinationIdsJson, &destinationIDs); err != nil {
		return notificationstate.PendingNotification{}, fmt.Errorf("decode destinations: %w", err)
	}
	if err := json.Unmarshal(row.DeliveredDestinationIdsJson, &deliveredDestinations); err != nil {
		return notificationstate.PendingNotification{}, fmt.Errorf("decode delivered destinations: %w", err)
	}
	if err := json.Unmarshal(row.FailedDestinationIdsJson, &failedDestinations); err != nil {
		return notificationstate.PendingNotification{}, fmt.Errorf("decode failed destinations: %w", err)
	}
	pending := notificationstate.PendingNotification{
		NotificationID:          row.NotificationID,
		CorporationID:           row.CorporationID,
		CorporationName:         row.CorporationName,
		CorporationTicker:       row.CorporationTicker,
		CharacterID:             row.CharacterID,
		NotificationJSON:        append([]byte(nil), row.NotificationJson...),
		DestinationIDs:          destinationIDs,
		DeliveredDestinationIDs: deliveredDestinations,
		FailedDestinationIDs:    failedDestinations,
		Attempts:                int(row.Attempts),
		CreatedAt:               time.Unix(row.CreatedAt, 0).UTC(),
		NextRetryAt:             time.Unix(row.NextRetryAt, 0).UTC(),
		LastError:               row.LastError,
	}
	if err := pending.Validate(); err != nil {
		return notificationstate.PendingNotification{}, err
	}
	return pending, nil
}

// ReschedulePending updates retry metadata for one pending notification.
func (s *Store) ReschedulePending(ctx context.Context, pending *notificationstate.PendingNotification) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	if pending == nil {
		return errors.New("pending notification is required")
	}
	if err := validatePending(pending); err != nil {
		return err
	}
	failedDestinations, err := json.Marshal(pending.FailedDestinationIDs)
	if err != nil {
		return fmt.Errorf("encode pending notification destinations: %w", err)
	}
	deliveredDestinations, err := json.Marshal(pending.DeliveredDestinationIDs)
	if err != nil {
		return fmt.Errorf("encode delivered notification destinations: %w", err)
	}
	rows, err := s.queries.ReschedulePending(ctx, gen.ReschedulePendingParams{
		DeliveredDestinationIdsJson: deliveredDestinations,
		FailedDestinationIdsJson:    failedDestinations,
		Attempts:                    int64(pending.Attempts),
		NextRetryAt:                 pending.NextRetryAt.Unix(),
		LastError:                   pending.LastError,
		NotificationID:              pending.NotificationID,
	})
	if err != nil {
		return fmt.Errorf("reschedule pending notification: %w", err)
	}
	if rows != 1 {
		return errors.New("pending notification not found")
	}
	return nil
}

// DeletePending removes a completed or permanently failed notification retry.
func (s *Store) DeletePending(ctx context.Context, notificationID int64) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	if err := s.queries.DeletePending(ctx, notificationID); err != nil {
		return fmt.Errorf("delete pending notification: %w", err)
	}
	return nil
}

// PruneSeen removes globally seen IDs older than before.
func (s *Store) PruneSeen(ctx context.Context, before time.Time) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	if err := s.queries.PruneSeen(ctx, before.Unix()); err != nil {
		return fmt.Errorf("prune seen notifications: %w", err)
	}
	return nil
}

// RecordAlert stores one terminal Discord delivery outcome and prunes history
// older than the supplied retention policy.
func (s *Store) RecordAlert(ctx context.Context, record *notificationstate.AlertHistoryRecord) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	if record == nil {
		return errors.New("alert history record is required")
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if len(record.RawNotificationJSON) > maxHistoryPayloadBytes ||
		len(record.ClassifiedEventJSON) > maxHistoryPayloadBytes ||
		len(record.DiscordPayloadJSON) > maxHistoryPayloadBytes {
		return errors.New("alert history payload exceeds size limit")
	}
	if len(record.DeliveryError) > maxHistoryErrorBytes {
		return errors.New("alert history delivery error exceeds size limit")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin alert history record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	if err := queries.InsertAlertHistory(ctx, gen.InsertAlertHistoryParams{
		NotificationID:      record.NotificationID,
		NotificationType:    record.NotificationType,
		AlertType:           record.AlertType,
		CorporationID:       record.CorporationID,
		CorporationName:     record.CorporationName,
		CorporationTicker:   record.CorporationTicker,
		CharacterID:         record.CharacterID,
		DestinationID:       record.DestinationID,
		DeliveryStatus:      record.DeliveryStatus,
		DeliveryError:       record.DeliveryError,
		DispatchedAt:        record.DispatchedAt.Unix(),
		RawNotificationJson: append([]byte(nil), record.RawNotificationJSON...),
		ClassifiedEventJson: append([]byte(nil), record.ClassifiedEventJSON...),
		DiscordPayloadJson:  append([]byte(nil), record.DiscordPayloadJSON...),
	}); err != nil {
		return fmt.Errorf("insert alert history: %w", err)
	}
	if err := queries.DeleteAlertHistoryBefore(ctx, time.Now().UTC().Add(-alertHistoryRetention).Unix()); err != nil {
		return fmt.Errorf("prune alert history: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alert history: %w", err)
	}
	return nil
}

// ListAlertHistory returns recent alert deliveries at or after since, newest
// first, up to limit records.
func (s *Store) ListAlertHistory(ctx context.Context, since time.Time, limit int) ([]notificationstate.AlertHistoryRecord, error) {
	if err := s.validateContext(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	rows, err := s.queries.ListAlertHistory(ctx, gen.ListAlertHistoryParams{DispatchedAt: since.Unix(), Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list alert history: %w", err)
	}
	items := make([]notificationstate.AlertHistoryRecord, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		if len(row.RawNotificationJson) > maxHistoryPayloadBytes ||
			len(row.ClassifiedEventJson) > maxHistoryPayloadBytes ||
			len(row.DiscordPayloadJson) > maxHistoryPayloadBytes {
			return nil, errors.New("alert history payload exceeds size limit")
		}
		if len(row.DeliveryError) > maxHistoryErrorBytes {
			return nil, errors.New("alert history delivery error exceeds size limit")
		}
		items = append(items, notificationstate.AlertHistoryRecord{
			NotificationID:      row.NotificationID,
			NotificationType:    row.NotificationType,
			AlertType:           row.AlertType,
			CorporationID:       row.CorporationID,
			CorporationName:     row.CorporationName,
			CorporationTicker:   row.CorporationTicker,
			CharacterID:         row.CharacterID,
			DestinationID:       row.DestinationID,
			DeliveryStatus:      row.DeliveryStatus,
			DeliveryError:       row.DeliveryError,
			DispatchedAt:        time.Unix(row.DispatchedAt, 0).UTC(),
			RawNotificationJSON: append([]byte(nil), row.RawNotificationJson...),
			ClassifiedEventJSON: append([]byte(nil), row.ClassifiedEventJson...),
			DiscordPayloadJSON:  append([]byte(nil), row.DiscordPayloadJson...),
		})
	}
	return items, nil
}

// PruneAlertHistory removes records older than before.
func (s *Store) PruneAlertHistory(ctx context.Context, before time.Time) error {
	if err := s.validateContext(ctx); err != nil {
		return err
	}
	if err := s.queries.DeleteAlertHistoryBefore(ctx, before.Unix()); err != nil {
		return fmt.Errorf("delete expired alert history: %w", err)
	}
	return nil
}

func (s *Store) validateContext(ctx context.Context) error {
	if s == nil || s.db == nil || s.queries == nil {
		return errors.New("notification store is unavailable")
	}
	if ctx == nil {
		return errors.New("notification store context is required")
	}
	return ctx.Err()
}

func (s *Store) admitTransaction(ctx context.Context, admission *notificationstate.Admission, cursorJSON []byte, pendingData *encodedPending) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin notification admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	result, err := queries.MarkSeen(ctx, admission.NotificationID)
	if err != nil {
		return false, fmt.Errorf("mark notification seen during admission: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect notification admission: %w", err)
	}
	if rows == 1 && admission.Pending != nil {
		if err := insertPending(ctx, queries, admission.Pending, pendingData); err != nil {
			return false, fmt.Errorf("store pending notification: %w", err)
		}
	}
	if err := queries.SaveCursor(ctx, gen.SaveCursorParams{CorporationID: admission.CorporationID, StateJson: cursorJSON}); err != nil {
		return false, fmt.Errorf("save notification cursor with admission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit notification admission: %w", err)
	}
	return rows == 1 && admission.Pending != nil, nil
}

func validatePending(pending *notificationstate.PendingNotification) error {
	return pending.Validate()
}

func (s *Store) dropMalformedPending(ctx context.Context, notificationID int64, reason error) error {
	if err := s.queries.DeletePending(ctx, notificationID); err != nil {
		return errors.Join(
			fmt.Errorf("drop malformed pending notification %d: %w", notificationID, reason),
			fmt.Errorf("delete malformed pending notification %d: %w", notificationID, err),
		)
	}
	return nil
}

func withDefaults(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
}
