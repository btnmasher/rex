package main

import (
	"testing"

	"github.com/btnmasher/rex/internal/notifications"
)

func TestSyntheticEventsCoverAllAlertTypes(t *testing.T) {
	events, err := syntheticEvents()
	if err != nil {
		t.Fatalf("synthetic events: %v", err)
	}

	covered := make(map[string]struct{}, len(events))
	for i := range events {
		covered[events[i].AlertType] = struct{}{}
	}
	for _, alertType := range notifications.AllAlertTypes() {
		if _, ok := covered[alertType]; !ok {
			t.Errorf("synthetic events do not cover %q", alertType)
		}
	}
}
