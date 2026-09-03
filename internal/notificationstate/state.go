// Package notificationstate defines durable notification cursor state.
package notificationstate

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

const maxPendingNotificationBytes = 256 << 10

// Stream is the per-character ESI cursor within one corporation.
type Stream struct {
	LastTimestamp time.Time          `json:"lastTimestamp"`
	SeenAtCursor  map[int64]struct{} `json:"seenAtCursor"`
}

// Cursor is the durable per-corporation ESI stream state.
type Cursor struct {
	Streams map[string]Stream `json:"streams"`
}

// PendingNotification is a globally claimed notification awaiting delivery.
// The serialized notification and failed destination IDs make retries survive
// process restarts without retaining access tokens or external clients.
type PendingNotification struct {
	NotificationID       int64
	CorporationID        string
	CorporationName      string
	CorporationTicker    string
	CharacterID          string
	NotificationJSON     []byte
	FailedDestinationIDs []string
	Attempts             int
	CreatedAt            time.Time
	NextRetryAt          time.Time
	LastError            string
}

// AlertHistoryRecord captures one accepted Discord alert delivery without
// retaining webhook credentials or EVE access tokens.
type AlertHistoryRecord struct {
	NotificationID      int64
	NotificationType    string
	AlertType           string
	CorporationID       string
	CorporationName     string
	CorporationTicker   string
	CharacterID         string
	DestinationID       string
	DispatchedAt        time.Time
	RawNotificationJSON []byte
	ClassifiedEventJSON []byte
	DiscordPayloadJSON  []byte
}

// AlertHistory persists and lists successfully dispatched alert payloads.
type AlertHistory interface {
	RecordAlert(context.Context, *AlertHistoryRecord) error
	ListAlertHistory(context.Context, time.Time, int) ([]AlertHistoryRecord, error)
}

// Validate checks the identity, timestamp, and payload fields required by an
// alert history record.
func (r *AlertHistoryRecord) Validate() error {
	if r == nil {
		return errors.New("alert history record is required")
	}
	if r.NotificationID <= 0 || strings.TrimSpace(r.NotificationType) == "" || strings.TrimSpace(r.AlertType) == "" {
		return errors.New("alert history notification identity is incomplete")
	}
	if strings.TrimSpace(r.CorporationID) == "" || strings.TrimSpace(r.CharacterID) == "" || strings.TrimSpace(r.DestinationID) == "" {
		return errors.New("alert history delivery identity is incomplete")
	}
	if r.DispatchedAt.IsZero() {
		return errors.New("alert history dispatch time is required")
	}
	if len(r.RawNotificationJSON) == 0 || len(r.ClassifiedEventJSON) == 0 || len(r.DiscordPayloadJSON) == 0 {
		return errors.New("alert history payloads are required")
	}
	return nil
}

// Validate checks the identity and retry fields required by every store.
func (p *PendingNotification) Validate() error {
	if p == nil {
		return errors.New("pending notification is required")
	}
	if p.NotificationID <= 0 || strings.TrimSpace(p.CorporationID) == "" || strings.TrimSpace(p.CharacterID) == "" {
		return errors.New("pending notification identity is incomplete")
	}
	if len(p.NotificationJSON) == 0 {
		return errors.New("pending notification payload is required")
	}
	if len(p.NotificationJSON) > maxPendingNotificationBytes {
		return ErrPendingNotificationPayloadTooLarge
	}
	if p.Attempts < 0 || p.NextRetryAt.IsZero() {
		return errors.New("pending notification retry state is invalid")
	}
	return nil
}

// Store persists notification cursor state by corporation.
type Store interface {
	Load(context.Context, string) (Cursor, error)
	Save(context.Context, string, Cursor) error
	MarkSeen(context.Context, int64) (bool, error)
	ClaimPending(context.Context, *PendingNotification) (bool, error)
	ListPending(context.Context, int, time.Time) ([]PendingNotification, error)
	ReschedulePending(context.Context, *PendingNotification) error
	DeletePending(context.Context, int64) error
	PruneSeen(context.Context, time.Time) error
}

// MemoryStore provides the process-local fallback used by unit tests and
// callers that do not require restart durability.
type MemoryStore struct {
	mu      sync.Mutex
	values  map[string]Cursor
	seen    map[int64]time.Time
	pending map[int64]PendingNotification
}

// NewMemoryStore creates an empty process-local cursor store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{values: make(map[string]Cursor), seen: make(map[int64]time.Time), pending: make(map[int64]PendingNotification)}
}

// Load returns a cloned cursor or an empty cursor when none exists.
func (s *MemoryStore) Load(ctx context.Context, corporationID string) (Cursor, error) {
	if err := ctx.Err(); err != nil {
		return Cursor{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.values[corporationID]
	if !ok {
		return Cursor{}, nil
	}
	return clone(value), nil
}

// Save replaces one corporation's cursor.
func (s *MemoryStore) Save(ctx context.Context, corporationID string, value Cursor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[corporationID] = clone(value)
	return nil
}

// MarkSeen atomically claims a globally unique notification ID.
func (s *MemoryStore) MarkSeen(ctx context.Context, notificationID int64) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[notificationID]; ok {
		return false, nil
	}
	s.seen[notificationID] = time.Now().UTC()
	return true, nil
}

// ClaimPending atomically claims a notification and stores its retry payload.
func (s *MemoryStore) ClaimPending(ctx context.Context, pending *PendingNotification) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := pending.Validate(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[pending.NotificationID]; ok {
		return false, nil
	}
	now := time.Now().UTC()
	s.seen[pending.NotificationID] = now
	pendingCopy := clonePending(pending)
	if pendingCopy.CreatedAt.IsZero() {
		pendingCopy.CreatedAt = now
	}
	s.pending[pending.NotificationID] = pendingCopy
	return true, nil
}

// ListPending returns due retry payloads in deterministic order.
func (s *MemoryStore) ListPending(ctx context.Context, limit int, now time.Time) ([]PendingNotification, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]PendingNotification, 0, len(s.pending))
	for notificationID := range s.pending {
		pending := s.pending[notificationID]
		if !pending.NextRetryAt.After(now) {
			items = append(items, clonePending(&pending))
		}
	}
	sortPending(items)
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

// ReschedulePending updates retry metadata for one pending notification.
func (s *MemoryStore) ReschedulePending(ctx context.Context, pending *PendingNotification) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pending.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[pending.NotificationID]; !ok {
		return errors.New("pending notification not found")
	}
	s.pending[pending.NotificationID] = clonePending(pending)
	return nil
}

// DeletePending removes a completed or permanently failed notification retry.
func (s *MemoryStore) DeletePending(ctx context.Context, notificationID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, notificationID)
	return nil
}

// PruneSeen removes IDs older than the supplied retention boundary.
func (s *MemoryStore) PruneSeen(ctx context.Context, before time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for notificationID, seenAt := range s.seen {
		if seenAt.Before(before) {
			if _, pending := s.pending[notificationID]; pending {
				continue
			}
			delete(s.seen, notificationID)
		}
	}
	return nil
}

func clonePending(value *PendingNotification) PendingNotification {
	if value == nil {
		return PendingNotification{}
	}
	clone := *value
	clone.NotificationJSON = append([]byte(nil), value.NotificationJSON...)
	clone.FailedDestinationIDs = append([]string(nil), value.FailedDestinationIDs...)
	return clone
}

func sortPending(items []PendingNotification) {
	slices.SortFunc(items, func(left, right PendingNotification) int {
		if left.NextRetryAt.Before(right.NextRetryAt) {
			return -1
		}
		if left.NextRetryAt.After(right.NextRetryAt) {
			return 1
		}
		return cmp.Compare(left.NotificationID, right.NotificationID)
	})
}

func clone(value Cursor) Cursor {
	cloned := Cursor{Streams: make(map[string]Stream, len(value.Streams))}
	for characterID, stream := range value.Streams {
		seen := make(map[int64]struct{}, len(stream.SeenAtCursor))
		for notificationID := range stream.SeenAtCursor {
			seen[notificationID] = struct{}{}
		}
		cloned.Streams[characterID] = Stream{LastTimestamp: stream.LastTimestamp, SeenAtCursor: seen}
	}
	return cloned
}
