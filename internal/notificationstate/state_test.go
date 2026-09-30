package notificationstate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPendingNotificationRejectsOversizedPayload(t *testing.T) {
	pending := &PendingNotification{
		NotificationID:   1,
		CorporationID:    "100",
		CharacterID:      "200",
		NotificationJSON: make([]byte, maxPendingNotificationBytes+1),
		NextRetryAt:      time.Now().UTC().Add(time.Minute),
	}
	if err := pending.Validate(); !errors.Is(err, ErrPendingNotificationPayloadTooLarge) {
		t.Fatalf("validate oversized pending notification: %v", err)
	}
}

func TestMemoryStoreRejectsInvalidPendingReschedule(t *testing.T) {
	store := NewMemoryStore()
	pending := &PendingNotification{
		NotificationID:   1,
		CorporationID:    "100",
		CharacterID:      "200",
		NotificationJSON: []byte(`{"type":"StructureUnderAttack"}`),
		NextRetryAt:      time.Now().UTC().Add(time.Minute),
	}
	if claimed, err := store.Admit(context.Background(), &Admission{NotificationID: pending.NotificationID, CorporationID: pending.CorporationID, Pending: pending}); err != nil || !claimed {
		t.Fatalf("claim pending: claimed=%t err=%v", claimed, err)
	}
	pending.NotificationJSON = nil
	if err := store.ReschedulePending(context.Background(), pending); err == nil {
		t.Fatal("expected invalid pending notification to be rejected")
	}
}

func TestMemoryStorePrunesSkippedNotificationsAndClonesPayload(t *testing.T) {
	store := NewMemoryStore()
	old := SkippedNotification{NotificationID: 1, Reason: "old", SkippedAt: time.Now().UTC().Add(-SkippedNotificationRetention - time.Second), RawNotificationJSON: []byte(`{"old":true}`)}
	store.skipped = append(store.skipped, old)
	record := &SkippedNotification{NotificationID: 2, Reason: "malformed", SkippedAt: time.Now().UTC(), RawNotificationJSON: []byte(`{"text":"raw"}`)}
	if err := store.RecordSkippedNotification(context.Background(), record); err != nil {
		t.Fatalf("record skipped notification: %v", err)
	}
	record.RawNotificationJSON[0] = 'X'
	if len(store.skipped) != 1 || store.skipped[0].NotificationID != 2 {
		t.Fatalf("unexpected skipped notifications: %#v", store.skipped)
	}
	if string(store.skipped[0].RawNotificationJSON) != `{"text":"raw"}` {
		t.Fatalf("payload was not cloned: %s", store.skipped[0].RawNotificationJSON)
	}
}
