package enrichment

import (
	"context"
	"errors"
	"testing"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/universe"
)

type enrichmentDatabase struct {
	structures []authnextdb.Structure
	ids        []string
}

func (d *enrichmentDatabase) GetStructuresByIDs(_ context.Context, ids []string) ([]authnextdb.Structure, error) {
	d.ids = append([]string(nil), ids...)
	return append([]authnextdb.Structure(nil), d.structures...), nil
}

type enrichmentResolver struct{}

func (enrichmentResolver) ResolveAlliance(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected alliance lookup")
}

func (enrichmentResolver) ResolveCharacter(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected character lookup")
}

func (enrichmentResolver) ResolveCorporation(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected corporation lookup")
}

func (enrichmentResolver) ResolveRegion(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected region lookup")
}

func (enrichmentResolver) ResolveSolarSystem(context.Context, string) (universe.SolarSystem, error) {
	return universe.SolarSystem{
		Name:       "Jita",
		RegionID:   "10000002",
		RegionName: "The Forge",
	}, nil
}

func (enrichmentResolver) ResolveStructureType(context.Context, string) (universe.Entity, error) {
	return universe.Entity{Name: "Athanor"}, nil
}

func (enrichmentResolver) ResolvePlanet(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected planet lookup")
}

func (enrichmentResolver) ResolveMoon(context.Context, string) (universe.Entity, error) {
	return universe.Entity{}, errors.New("unexpected moon lookup")
}

func TestEnrichUsesPayloadOwnerAndGlobalStructureLookup(t *testing.T) {
	database := &enrichmentDatabase{structures: []authnextdb.Structure{{
		ID:       "42",
		TypeID:   "35834",
		SystemID: "30000142",
	}}}
	service := NewService(database, enrichmentResolver{}, nil)
	envelope := &Envelope{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corp", Ticker: "POLL"},
		CharacterID: "200",
		Event: notifications.Event{
			NotificationID:       900,
			NotificationType:     "StructureUnderAttack",
			AlertType:            notifications.AlertStructureUnderAttack,
			StructureIDs:         []string{"42"},
			SystemID:             "30000142",
			StructureTypeID:      "35834",
			OwnerCorporationID:   "300",
			OwnerCorporationName: "Payload Owner",
			AllianceName:         "Tracked Alliance",
		},
	}

	view, err := service.Enrich(context.Background(), envelope)
	if err != nil {
		t.Fatalf("enrich notification: %v", err)
	}
	if len(database.ids) != 1 || database.ids[0] != "42" {
		t.Fatalf("structure IDs = %#v, want [42]", database.ids)
	}
	if view.CorporationID != "300" || view.CorporationName != "Payload Owner" {
		t.Fatalf("owner = %s %q, want payload owner", view.CorporationID, view.CorporationName)
	}
	if view.PollingCorporationID != "100" || view.SystemName != "Jita" || view.RegionName != "The Forge" {
		t.Fatalf("enriched context = %+v", view)
	}
	if view.StructureTypeName != "Athanor" || len(view.Structures) != 1 || view.Structures[0].RegionName == nil || *view.Structures[0].RegionName != "The Forge" {
		t.Fatalf("structure enrichment = %+v", view.Structures)
	}
}

func TestEnrichResolvesMoonminingOreNames(t *testing.T) {
	view, err := NewService(nil, enrichmentResolver{}, nil).Enrich(context.Background(), &Envelope{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corp"},
		Event:       notifications.Event{OreComposition: []notifications.OreComposition{{TypeID: "45498", Volume: 10}}},
	})
	if err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if view.OreTypeNames["45498"] != "Athanor" {
		t.Fatalf("ore type names = %#v", view.OreTypeNames)
	}
}

func TestEnrichRemainsUsableWithoutOptionalSources(t *testing.T) {
	service := NewService(nil, nil, nil)
	view, err := service.Enrich(context.Background(), &Envelope{
		Corporation: authnextdb.Corporation{ID: "100", Name: "Polling Corp"},
		CharacterID: "200",
		Event: notifications.Event{
			NotificationID:   901,
			NotificationType: "SovCommandNodeEventStarted",
			AlertType:        notifications.AlertSovCommandNodeEventStarted,
			AllianceID:       "400",
			AllianceName:     "Known Alliance",
		},
	})
	if err != nil {
		t.Fatalf("enrich without optional sources: %v", err)
	}
	if view.AllianceName != "Known Alliance" || view.PollingCorporationID != "100" {
		t.Fatalf("partial context = %+v", view)
	}
}
