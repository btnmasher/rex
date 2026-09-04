package routing

import (
	"testing"

	"github.com/btnmasher/rex/internal/enrichment"
	"github.com/btnmasher/rex/internal/notifications"
)

func TestPolicyRejectsDuplicateTargetIDsAcrossDestinations(t *testing.T) {
	_, err := New([]Destination{
		{ID: "military", TargetIDs: []string{"target-1"}, Filters: Filters{AlertTypes: []string{notifications.AlertSkyhook}}},
		{ID: "operations", TargetIDs: []string{"target-1", "target-2"}, Filters: Filters{AlertTypes: []string{notifications.AlertSkyhook}}},
	})
	if err == nil {
		t.Fatal("expected duplicate target IDs to be rejected")
	}
}

func TestPolicyExclusionsOverrideGroupAndCorporationInclusion(t *testing.T) {
	policy, err := New([]Destination{{
		ID:        "military",
		TargetIDs: []string{"target-1"},
		Filters: Filters{
			AlertTypes:            []string{notifications.AlertStructureCombat},
			ExcludeAlertTypes:     []string{notifications.AlertStructureDestroyed},
			IncludeCorporationIDs: []string{"100"},
			ExcludeCorporationIDs: []string{"100"},
		},
	}})
	if err != nil {
		t.Fatalf("new policy: %v", err)
	}
	if got := policy.PreRoute("100", notifications.AlertStructureUnderAttack); len(got) != 0 {
		t.Fatalf("excluded corporation selected targets = %#v", got)
	}
	if got := policy.PreRoute("200", notifications.AlertStructureDestroyed); len(got) != 0 {
		t.Fatalf("excluded leaf selected targets = %#v", got)
	}
}

func TestPolicyPostRouteAppliesStructureTypeExclusion(t *testing.T) {
	policy, err := New([]Destination{{
		ID:        "military",
		TargetIDs: []string{"target-1"},
		Filters: Filters{
			AlertTypes:              []string{notifications.AlertStructureCombat},
			ExcludeStructureTypeIDs: []string{"85230"},
		},
	}})
	if err != nil {
		t.Fatalf("new policy: %v", err)
	}
	view := &enrichment.Context{PollingCorporationID: "100", Event: notifications.Event{
		AlertType:        notifications.AlertStructureUnderAttack,
		NotificationType: "StructureUnderAttack",
		StructureTypeID:  "85230",
	}}
	candidates := policy.PreRoute("100", view.Event.AlertType)
	if got := policy.PostRoute(view, candidates); len(got) != 0 {
		t.Fatalf("excluded structure type selected targets = %#v", got)
	}
}
