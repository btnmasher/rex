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

// NewStore opens or creates a durable cursor database and applies pending migrations using ctx.
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

// ClaimPending atomically claims a notification and stores its retry payload.
func (s *Store) ClaimPending(ctx context.Context, pending *notificationstate.PendingNotification) (bool, error) {
	if pending == nil {
		return false, errors.New("pending notification is required")
	}
	if err := validatePending(pending); err != nil {
		return false, err
	}
	failedDestinations, err := json.Marshal(pending.FailedDestinationIDs)
	if err != nil {
		return false, fmt.Errorf("encode pending notification destinations: %w", err)
	}
	createdAt := pending.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin pending notification claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	queries := s.queries.WithTx(tx)
	result, err := queries.MarkSeen(ctx, pending.NotificationID)
	if err != nil {
		return false, fmt.Errorf("mark pending notification seen: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect pending notification claim: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	if err := queries.InsertPending(ctx, gen.InsertPendingParams{
		NotificationID:           pending.NotificationID,
		CorporationID:            pending.CorporationID,
		CorporationName:          pending.CorporationName,
		CorporationTicker:        pending.CorporationTicker,
		CharacterID:              pending.CharacterID,
		NotificationJson:         pending.NotificationJSON,
		FailedDestinationIdsJson: failedDestinations,
		Attempts:                 int64(pending.Attempts),
		CreatedAt:                createdAt.Unix(),
		NextRetryAt:              pending.NextRetryAt.Unix(),
		LastError:                pending.LastError,
	}); err != nil {
		return false, fmt.Errorf("store pending notification: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit pending notification claim: %w", err)
	}
	return true, nil
}

// ListPending returns due retry payloads in deterministic order.
func (s *Store) ListPending(ctx context.Context, limit int, now time.Time) ([]notificationstate.PendingNotification, error) {
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
		var failedDestinations []string
		if err := json.Unmarshal(row.FailedDestinationIdsJson, &failedDestinations); err != nil {
			if err := s.dropMalformedPending(ctx, row.NotificationID, fmt.Errorf("decode destinations: %w", err)); err != nil {
				return nil, err
			}
			continue
		}
		pending := notificationstate.PendingNotification{
			NotificationID:       row.NotificationID,
			CorporationID:        row.CorporationID,
			CorporationName:      row.CorporationName,
			CorporationTicker:    row.CorporationTicker,
			CharacterID:          row.CharacterID,
			NotificationJSON:     append([]byte(nil), row.NotificationJson...),
			FailedDestinationIDs: failedDestinations,
			Attempts:             int(row.Attempts),
			CreatedAt:            time.Unix(row.CreatedAt, 0).UTC(),
			NextRetryAt:          time.Unix(row.NextRetryAt, 0).UTC(),
			LastError:            row.LastError,
		}
		if err := pending.Validate(); err != nil {
			if err := s.dropMalformedPending(ctx, row.NotificationID, err); err != nil {
				return nil, err
			}
			continue
		}
		items = append(items, pending)
	}
	return items, nil
}

// ReschedulePending updates retry metadata for one pending notification.
func (s *Store) ReschedulePending(ctx context.Context, pending *notificationstate.PendingNotification) error {
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
	rows, err := s.queries.ReschedulePending(ctx, gen.ReschedulePendingParams{
		FailedDestinationIdsJson: failedDestinations,
		Attempts:                 int64(pending.Attempts),
		NextRetryAt:              pending.NextRetryAt.Unix(),
		LastError:                pending.LastError,
		NotificationID:           pending.NotificationID,
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
	if err := s.queries.DeletePending(ctx, notificationID); err != nil {
		return fmt.Errorf("delete pending notification: %w", err)
	}
	return nil
}

// PruneSeen removes globally seen IDs older than before.
func (s *Store) PruneSeen(ctx context.Context, before time.Time) error {
	if err := s.queries.PruneSeen(ctx, before.Unix()); err != nil {
		return fmt.Errorf("prune seen notifications: %w", err)
	}
	return nil
}

// RecordAlert stores one successfully accepted Discord payload and prunes
// history older than the supplied retention policy.
func (s *Store) RecordAlert(ctx context.Context, record *notificationstate.AlertHistoryRecord) error {
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
		items = append(items, notificationstate.AlertHistoryRecord{
			NotificationID:      row.NotificationID,
			NotificationType:    row.NotificationType,
			AlertType:           row.AlertType,
			CorporationID:       row.CorporationID,
			CorporationName:     row.CorporationName,
			CorporationTicker:   row.CorporationTicker,
			CharacterID:         row.CharacterID,
			DestinationID:       row.DestinationID,
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
	if err := s.queries.DeleteAlertHistoryBefore(ctx, before.Unix()); err != nil {
		return fmt.Errorf("delete expired alert history: %w", err)
	}
	return nil
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
