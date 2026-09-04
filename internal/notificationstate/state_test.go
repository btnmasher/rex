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
