package sqlite

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/btnmasher/rex/internal/notificationstate"
)

func TestStorePersistsCorporationCursorAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notification-state.sqlite")
	want := notificationstate.Cursor{
		Streams: map[string]notificationstate.Stream{
			"900000001": {LastTimestamp: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), SeenAtCursor: map[int64]struct{}{123: {}}},
		},
	}
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if err := store.Save(context.Background(), "100", want); err != nil {
		t.Fatalf("save cursor: %v", err)
	}
	seen, err := store.MarkSeen(context.Background(), 123)
	if err != nil {
		t.Fatalf("mark notification seen: %v", err)
	}
	if !seen {
		t.Fatal("expected first global notification claim to succeed")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.Load(context.Background(), "100")
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	if got.Streams["900000001"].LastTimestamp != want.Streams["900000001"].LastTimestamp {
		t.Fatalf("unexpected stream timestamp: got %v want %v", got.Streams["900000001"].LastTimestamp, want.Streams["900000001"].LastTimestamp)
	}
	seen, err = store.MarkSeen(context.Background(), 123)
	if err != nil {
		t.Fatalf("reclaim notification: %v", err)
	}
	if seen {
		t.Fatal("expected global notification claim to persist across reopen")
	}
}

func TestStorePersistsPendingNotificationAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notification-state.sqlite")
	pending := notificationstate.PendingNotification{
		NotificationID:       456,
		CorporationID:        "100",
		CorporationName:      "Corp",
		CorporationTicker:    "CORP",
		CharacterID:          "900000001",
		NotificationJSON:     []byte(`{"notification_id":456}`),
		FailedDestinationIDs: []string{"destination-b"},
		Attempts:             2,
		NextRetryAt:          time.Now().UTC().Add(-time.Second),
		LastError:            "temporary failure",
	}
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	claimed, err := store.Admit(context.Background(), &notificationstate.Admission{
		NotificationID: pending.NotificationID,
		CorporationID:  pending.CorporationID,
		Pending:        &pending,
	})
	if err != nil || !claimed {
		t.Fatalf("claim pending: claimed=%v err=%v", claimed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	store, err = NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()
	got, err := store.ListPending(context.Background(), 10, time.Now().UTC())
	if err != nil || len(got) != 1 {
		t.Fatalf("list pending: pending=%v err=%v", got, err)
	}
	if got[0].NotificationID != pending.NotificationID ||
		got[0].CorporationID != pending.CorporationID ||
		len(got[0].FailedDestinationIDs) != 1 ||
		got[0].FailedDestinationIDs[0] != "destination-b" {
		t.Fatalf("unexpected pending notification: %+v", got[0])
	}
}

func TestStoreDropsMalformedPendingNotificationWhenListing(t *testing.T) {
	store, err := NewStore(context.Background(), filepath.Join(t.TempDir(), "notification-state.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	if _, err := store.db.ExecContext(ctx, `
		insert into notification_retries (
		  notification_id, corporation_id, corporation_name, corporation_ticker,
		  character_id, notification_json, destination_ids_json,
		  failed_destination_ids_json, attempts, next_retry_at, last_error,
		  created_at, updated_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch())`,
		901, "100", "Corp", "CORP", "900000001", []byte(`{"notification_id":901}`),
		[]byte(`[]`), []byte(`not-json`), 0, time.Now().UTC().Add(-time.Second).Unix(), "malformed", time.Now().UTC().Unix()); err != nil {
		t.Fatalf("insert malformed pending notification: %v", err)
	}
	items, err := store.ListPending(ctx, 10, time.Now().UTC())
	if err != nil {
		t.Fatalf("list malformed pending notification: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("listed malformed pending notifications: %#v", items)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "select count(*) from notification_retries where notification_id = ?", 901).Scan(&count); err != nil {
		t.Fatalf("count malformed pending notification: %v", err)
	}
	if count != 0 {
		t.Fatalf("malformed pending notification count = %d, want 0", count)
	}
}

func TestStorePrunesAlertHistoryAfterInsertion(t *testing.T) {
	store, err := NewStore(context.Background(), filepath.Join(t.TempDir(), "notification-state.sqlite"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `
		insert into alert_history (
		  notification_id, notification_type, alert_type, corporation_id,
		  corporation_name, corporation_ticker, character_id, destination_id,
		  delivery_status, delivery_error, dispatched_at, raw_notification_json,
		  classified_event_json, discord_payload_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		902, "StructureDestroyed", notificationstateAlertType, "100", "Corp", "CORP", "900000001", "structure-alerts",
		notificationstate.AlertDeliveryStatusDelivered, "", old.Unix(), []byte(`{}`), []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatalf("insert old alert history: %v", err)
	}
	if err := store.RecordAlert(ctx, &notificationstate.AlertHistoryRecord{
		NotificationID:      903,
		NotificationType:    "StructureUnderAttack",
		AlertType:           notificationstateAlertType,
		CorporationID:       "100",
		CorporationName:     "Corp",
		CorporationTicker:   "CORP",
		CharacterID:         "900000001",
		DestinationID:       "structure-alerts",
		DeliveryStatus:      notificationstate.AlertDeliveryStatusDelivered,
		DispatchedAt:        time.Now().UTC(),
		RawNotificationJSON: []byte(`{"notification_id":903}`),
		ClassifiedEventJSON: []byte(`{"notification_id":903}`),
		DiscordPayloadJSON:  []byte(`{"embeds":[]}`),
	}); err != nil {
		t.Fatalf("record current alert history: %v", err)
	}
	items, err := store.ListAlertHistory(ctx, time.Unix(0, 0), 10)
	if err != nil {
		t.Fatalf("list alert history: %v", err)
	}
	if len(items) != 1 || items[0].NotificationID != 903 {
		t.Fatalf("retained alert history: %#v", items)
	}
}

func TestStoreRecordsAppliedSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notification-state.sqlite")
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	defer func() { _ = store.Close() }()

	var count int
	if err := store.db.QueryRowContext(context.Background(), "select count(*) from goose_db_version").Scan(&count); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if count == 0 {
		t.Fatal("expected at least one applied schema migration")
	}
}

func TestStoreRejectsUnknownAppliedMigrationVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notification-state.sqlite")
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		"insert into goose_db_version (version_id, is_applied) values (?, 1)", 999); err != nil {
		_ = store.Close()
		t.Fatalf("insert unknown migration: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if _, err := NewStore(context.Background(), path); err == nil {
		t.Fatal("expected unknown migration version to be rejected")
	}
}

func TestStorePersistsAlertHistoryAndPrunesItOnReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notification-state.sqlite")
	store, err := NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	ctx := context.Background()
	want := notificationstate.AlertHistoryRecord{
		NotificationID:      789,
		NotificationType:    "StructureUnderAttack",
		AlertType:           notificationstateAlertType,
		CorporationID:       "100",
		CorporationName:     "Corp",
		CorporationTicker:   "CORP",
		CharacterID:         "900000001",
		DestinationID:       "structure-alerts",
		DeliveryStatus:      notificationstate.AlertDeliveryStatusDelivered,
		DispatchedAt:        time.Now().UTC(),
		RawNotificationJSON: []byte(`{"notification_id":789,"text":"raw"}`),
		ClassifiedEventJSON: []byte(`{"notification_id":789,"alert_type":"structures.combat.under_attack"}`),
		DiscordPayloadJSON:  []byte(`{"embeds":[{"title":"Structure Under Attack"}]}`),
	}
	if err := store.RecordAlert(ctx, &want); err != nil {
		t.Fatalf("record alert history: %v", err)
	}
	got, err := store.ListAlertHistory(ctx, want.DispatchedAt.Add(-time.Minute), 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("list alert history: records=%v err=%v", got, err)
	}
	if got[0].NotificationID != want.NotificationID || got[0].DestinationID != want.DestinationID ||
		got[0].DeliveryStatus != want.DeliveryStatus || got[0].DeliveryError != want.DeliveryError ||
		!bytes.Equal(got[0].RawNotificationJSON, want.RawNotificationJSON) ||
		!bytes.Equal(got[0].DiscordPayloadJSON, want.DiscordPayloadJSON) {
		t.Fatalf("unexpected alert history record: %+v", got[0])
	}
	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `
		insert into alert_history (
		  notification_id, notification_type, alert_type, corporation_id,
		  corporation_name, corporation_ticker, character_id, destination_id,
		  delivery_status, delivery_error, dispatched_at, raw_notification_json,
		  classified_event_json, discord_payload_json
		) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		800, "StructureDestroyed", notificationstateAlertType, "100", "Corp", "CORP", "900000001", "structure-alerts",
		notificationstate.AlertDeliveryStatusDelivered, "", old.Unix(), []byte(`{}`), []byte(`{}`), []byte(`{}`)); err != nil {
		t.Fatalf("insert old alert history: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	store, err = NewStore(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = store.Close() }()
	got, err = store.ListAlertHistory(ctx, time.Unix(0, 0), 10)
	if err != nil || len(got) != 1 || got[0].NotificationID != want.NotificationID {
		t.Fatalf("unexpected retained history after prune: records=%v err=%v", got, err)
	}
}

const notificationstateAlertType = "structures.combat.under_attack"
