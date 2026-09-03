package notifications

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btnmasher/rex/internal/esi"
)

func TestClassifyStructureNotification(t *testing.T) {
	notification := esi.Notification{
		ID:        1,
		Type:      "StructureUnderAttack",
		Timestamp: time.Now(),
		Text:      `<b>StructureID: 123456789</b> system_id: 30000142`,
	}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected structure notification to classify")
	}
	if event.AlertType != AlertStructureUnderAttack {
		t.Fatalf("unexpected alert type %q", event.AlertType)
	}
	if len(event.StructureIDs) != 1 || event.StructureIDs[0] != "123456789" {
		t.Fatalf("unexpected structure IDs: %#v", event.StructureIDs)
	}
	if event.Text != "StructureID: 123456789 system_id: 30000142" {
		t.Fatalf("unexpected plain text: %q", event.Text)
	}
}

func TestClassifyIgnoresUnknownNotification(t *testing.T) {
	notification := esi.Notification{Type: "UnknownNotification"}
	if _, ok := Classify(&notification); ok {
		t.Fatal("expected unknown notification to be ignored")
	}
}

func TestAlertTaxonomyCoversEveryNotificationDefinition(t *testing.T) {
	seen := make(map[string]struct{})
	for notificationType, definition := range alertTypes {
		if !IsAlertGroup(definition.group) || !IsAlertType(definition.leaf) || !AlertTypeInGroup(definition.leaf, definition.group) {
			t.Fatalf("notification %q has invalid alert definition: %#v", notificationType, definition)
		}
		seen[definition.leaf] = struct{}{}
	}
	for group, types := range alertGroups {
		if len(types) == 0 {
			t.Fatalf("alert group %q has no leaf types", group)
		}
		for _, alertType := range types {
			if _, ok := seen[alertType]; !ok {
				t.Fatalf("alert group %q contains unmapped leaf type %q", group, alertType)
			}
		}
	}
}

func TestAlertSelectorHierarchy(t *testing.T) {
	tests := []struct {
		name       string
		selector   string
		want       []string
		wantAbsent string
	}{
		{
			name:       "structure state includes all state leaves",
			selector:   AlertStructureState,
			want:       []string{AlertStructureUnanchoring, AlertStructureVulnerable, AlertStructureOffline},
			wantAbsent: AlertStructureUnderAttack,
		},
		{
			name:     "structure combat includes attack and destruction",
			selector: AlertStructureCombat,
			want:     []string{AlertStructureUnderAttack, AlertStructureDestroyed},
		},
		{
			name:     "structure resources includes fuel and reagents",
			selector: AlertStructureFuel,
			want:     []string{AlertStructureFuelAlert, AlertStructureLowReagents, AlertStructureNoReagents},
		},
		{
			name:     "starbase includes tower leaves",
			selector: AlertStarbase,
			want:     []string{AlertStarbaseUnderAttack, AlertStarbaseResourceAlert},
		},
		{
			name:     "customs offices include orbital leaves",
			selector: AlertCustomsOffices,
			want:     []string{AlertCustomsOfficeAttacked, AlertCustomsOfficeReinforced},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ExpandAlertSelector(test.selector)
			if err != nil {
				t.Fatalf("expand selector: %v", err)
			}
			for _, want := range test.want {
				if !slices.Contains(got, want) {
					t.Fatalf("expanded %q = %#v, missing %q", test.selector, got, want)
				}
			}
			if slices.Contains(got, test.wantAbsent) {
				t.Fatalf("expanded %q unexpectedly contains %q", test.selector, test.wantAbsent)
			}
		})
	}
}

func TestNormalizeAlertSelector(t *testing.T) {
	got, err := NormalizeAlertSelector(" SKYHOOKS.* ")
	if err != nil || got != AlertSkyhook {
		t.Fatalf("normalize wildcard = %q, %v; want %q", got, err, AlertSkyhook)
	}
	if _, err := NormalizeAlertSelector("skyhooks.destroyed.*"); err == nil {
		t.Fatal("expected wildcard leaf selector to be rejected")
	}
}

func TestClassifyParsesStructureNotificationMetadata(t *testing.T) {
	notification := esi.Notification{
		ID:   2,
		Type: "StructureUnanchoring",
		Text: "Your structure is unanchoring. ownerCorpLinkData: - showinfo - 2 - 98790350 ownerCorpName: Unprofitable Ventures Inc. solarsystemID: 30002901 structureID: &id001 1055414089102 structureShowInfoData: - showinfo - 81826 - *id001 timeLeft: 2591993262378",
	}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected structure notification to classify")
	}
	if event.AlertType != AlertStructureUnanchoring {
		t.Fatalf("unexpected alert type %q", event.AlertType)
	}
	if event.OwnerCorporationID != "98790350" || event.OwnerCorporationName != "Unprofitable Ventures Inc." {
		t.Fatalf("unexpected owner corporation metadata: %#v", event)
	}
	if event.SystemID != "30002901" || event.StructureTypeID != "81826" || len(event.StructureIDs) != 1 || event.StructureIDs[0] != "1055414089102" {
		t.Fatalf("unexpected structure metadata: %#v", event)
	}
	if event.TimeLeft != time.Duration(2591993262378)*eveTickDuration {
		t.Fatalf("unexpected time left: %s", event.TimeLeft)
	}
	if event.Summary != "Your structure is unanchoring." {
		t.Fatalf("unexpected notification summary: %q", event.Summary)
	}
	if DisplayName(notification.Type) != "Structure Unanchoring" {
		t.Fatalf("unexpected display name: %q", DisplayName(notification.Type))
	}
}

func TestClassifyParsesSovereigntyClaimMetadata(t *testing.T) {
	notification := esi.Notification{
		ID:   3,
		Type: "SovAllClaimAquiredMsg",
		Text: "Your alliance has claimed sovereignty. allianceID: 99000001 corpID: 98790350 solarSystemID: 30000142",
	}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected sovereignty claim notification to classify")
	}
	if event.AlertType != AlertSovAllClaimAcquiredMsg {
		t.Fatalf("unexpected alert type %q", event.AlertType)
	}
	if event.AllianceID != "99000001" || event.OwnerCorporationID != "98790350" || event.SystemID != "30000142" {
		t.Fatalf("unexpected sovereignty claim metadata: %#v", event)
	}
	if event.Summary != "Your alliance has claimed sovereignty." {
		t.Fatalf("unexpected notification summary: %q", event.Summary)
	}
	if DisplayName(notification.Type) != "Sovereignty Claimed" {
		t.Fatalf("unexpected display name: %q", DisplayName(notification.Type))
	}
}

func TestClassifySeparatesVulnerableStructureNotifications(t *testing.T) {
	notification := esi.Notification{Type: "StructureVulnerable"}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected vulnerable structure notification to classify")
	}
	if event.AlertType != AlertStructureVulnerable {
		t.Fatalf("unexpected alert category %q", event.AlertType)
	}
	if !IsAlertCategory(AlertStructureState) || !IsAlertCategory(AlertStructureUnanchoring) || !IsAlertCategory(AlertStructureOffline) || !IsAlertCategory(AlertStructureOwnership) || !IsAlertCategory(AlertStructureVulnerable) {
		t.Fatal("expected structure state categories to be supported")
	}
}

func TestClassifySkyhookPayload(t *testing.T) {
	notification := esi.Notification{
		ID:        2446275910,
		Type:      "SkyhookUnderAttack",
		Timestamp: time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC),
		Text: "allianceID: 99013797\n" +
			"allianceName: The Caldari Fourth District\n" +
			"corpLinkData: - showinfo - 2 - 98774989\n" +
			"corpName: Fourth District Sentinels\n" +
			"charID: 2119766668\n" +
			"charID: 2119766668\n" +
			"itemID: &id001 1046813349750\n" +
			"planetID: 40126936\n" +
			"shieldPercentage: 94.59234682156118\n" +
			"armorPercentage: 100.0\n" +
			"hullPercentage: 100.0\n" +
			"solarsystemID: 30001988\n" +
			"typeID: 81080\n",
	}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected SkyhookUnderAttack to classify")
	}
	if event.AlertType != AlertSkyhookUnderAttack || event.StructureIDs[0] != "1046813349750" {
		t.Fatalf("unexpected skyhook event identity: %#v", event)
	}
	if event.AllianceName != "The Caldari Fourth District" || event.PlanetID != "40126936" || event.StructureTypeID != "81080" {
		t.Fatalf("unexpected skyhook location metadata: %#v", event)
	}
	if event.AttackerCharacterID != "2119766668" || event.AttackerCorporationID != "98774989" || event.AttackerCorporationName != "Fourth District Sentinels" {
		t.Fatalf("unexpected attacker metadata: %#v", event)
	}
	if event.ShieldPercentage == nil || *event.ShieldPercentage != 94.59234682156118 {
		t.Fatalf("unexpected shield percentage: %#v", event.ShieldPercentage)
	}
}

func TestClassifyIgnoresNonFiniteNumericMetadata(t *testing.T) {
	notification := esi.Notification{
		ID:   5,
		Type: "SkyhookUnderAttack",
		Text: "shieldPercentage: NaN armorPercentage: +Inf hullPercentage: -Inf solarsystemID: 30001988",
	}
	event, ok := Classify(&notification)
	if !ok {
		t.Fatal("expected notification to classify")
	}
	if event.ShieldPercentage != nil || event.ArmorPercentage != nil || event.HullPercentage != nil {
		t.Fatalf("non-finite numeric metadata was retained: %#v", event)
	}
}

func TestClassifyIgnoresReadState(t *testing.T) {
	base := esi.Notification{
		ID:        4,
		Type:      "StructureUnderAttack",
		Timestamp: time.Now(),
		Text:      "structureID: 1055414089102 solarSystemID: 30002901",
	}
	unread := base
	read := base
	read.IsRead = true
	unreadEvent, unreadOK := Classify(&unread)
	readEvent, readOK := Classify(&read)
	if !unreadOK || !readOK {
		t.Fatal("expected both read states to classify")
	}
	if unreadEvent.NotificationID != readEvent.NotificationID || unreadEvent.AlertType != readEvent.AlertType || unreadEvent.Text != readEvent.Text {
		t.Fatalf("read state changed classification: unread=%#v read=%#v", unreadEvent, readEvent)
	}
}

func TestClassifySkyhookLostShieldsParsesVulnerabilityWindow(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:        2446277929,
		Type:      "SkyhookLostShields",
		Timestamp: time.Date(2026, time.September, 2, 2, 11, 0, 0, time.UTC),
		Text: "itemID: &id001 1046813349750\n" +
			"planetID: 40126936\n" +
			"solarsystemID: 30001988\n" +
			"timeLeft: 1619860198926\n" +
			"typeID: 81080\n" +
			"vulnerableTime: 9000000000\n",
	})
	if !ok {
		t.Fatal("expected SkyhookLostShields to classify")
	}
	if event.StructureIDs[0] != "1046813349750" || event.TimeLeft != time.Duration(1619860198926)*eveTickDuration || event.VulnerableFor != 15*time.Minute {
		t.Fatalf("unexpected vulnerability metadata: %#v", event)
	}
}

func TestClassifyTowerAlert(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:   2446279000,
		Type: "TowerAlertMsg",
		Text: "aggressorAllianceID: 498125261\n" +
			"aggressorCorpID: 98774989\n" +
			"aggressorID: 2119766668\n" +
			"armorValue: 75.0\n" +
			"hullValue: 100.0\n" +
			"moonID: 40126937\n" +
			"shieldValue: 50.0\n" +
			"solarSystemID: 30000142\n" +
			"typeID: 35834\n",
	})
	if !ok {
		t.Fatal("expected tower alert to classify")
	}
	if event.AlertType != AlertStarbaseUnderAttack || event.SystemID != "30000142" || event.MoonID != "40126937" || event.StructureTypeID != "35834" {
		t.Fatalf("unexpected tower alert identity: %#v", event)
	}
	if event.AttackerAllianceID != "498125261" || event.AttackerCharacterID != "2119766668" || event.AttackerCorporationID != "98774989" {
		t.Fatalf("unexpected tower alert aggressor: %#v", event)
	}
	if event.ShieldValue == nil || *event.ShieldValue != 50 || event.ArmorValue == nil || *event.ArmorValue != 75 || event.HullValue == nil || *event.HullValue != 100 {
		t.Fatalf("unexpected tower alert integrity: %#v", event)
	}
}

func TestClassifyTowerResourceAlert(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:   2446279001,
		Type: "TowerResourceAlertMsg",
		Text: "allianceID: 99000001\ncorpID: 98790350\nmoonID: 40126937\n" +
			"solarSystemID: 30000142\ntypeID: 35834\n" +
			"wants:\n- quantity: 100\n  typeID: 4247\n- quantity: 20\n  typeID: 4246\n",
	})
	if !ok {
		t.Fatal("expected tower resource alert to classify")
	}
	if event.AlertType != AlertStarbaseResourceAlert || event.OwnerCorporationID != "98790350" || event.AllianceID != "99000001" {
		t.Fatalf("unexpected tower resource ownership: %#v", event)
	}
	if len(event.ResourceRequirements) != 2 || event.ResourceRequirements[0] != (ResourceRequirement{Quantity: 100, TypeID: "4247"}) || event.ResourceRequirements[1] != (ResourceRequirement{Quantity: 20, TypeID: "4246"}) {
		t.Fatalf("unexpected tower resource requirements: %#v", event.ResourceRequirements)
	}
}

func TestClassifyCustomsOfficeAttacked(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:   1,
		Type: "OrbitalAttacked",
		Text: "aggressorAllianceID: 498125261\n" +
			"aggressorCorpID: 98774989\n" +
			"aggressorID: 2119766668\n" +
			"planetID: 40126936\n" +
			"planetTypeID: 2015\n" +
			"shieldLevel: 0.945\n" +
			"solarSystemID: 30001988\n" +
			"typeID: 2233\n",
	})
	if !ok {
		t.Fatal("expected customs office attack to classify")
	}
	assertCustomsOfficeEvent(t, &event, AlertCustomsOfficeAttacked)
	if event.ShieldPercentage == nil || *event.ShieldPercentage != 94.5 {
		t.Fatalf("unexpected customs office shield level: %#v", event.ShieldPercentage)
	}
}

func TestClassifyCustomsOfficeReinforced(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:   1,
		Type: "OrbitalReinforced",
		Text: "aggressorAllianceID: 498125261\n" +
			"aggressorCorpID: 98774989\n" +
			"aggressorID: 2119766668\n" +
			"planetID: 40126936\n" +
			"planetTypeID: 2015\n" +
			"reinforceExitTime: 134329506560000000\n" +
			"solarSystemID: 30001988\n" +
			"typeID: 2233\n",
	})
	if !ok {
		t.Fatal("expected customs office reinforcement to classify")
	}
	assertCustomsOfficeEvent(t, &event, AlertCustomsOfficeReinforced)
	if event.ReinforcedUntil.IsZero() {
		t.Fatal("expected customs office reinforcement exit time")
	}
}

func assertCustomsOfficeEvent(t *testing.T, event *Event, alertType string) {
	t.Helper()
	if event == nil {
		t.Fatal("customs office event is nil")
	}
	if event.AlertType != alertType || event.SystemID != "30001988" || event.PlanetID != "40126936" || event.StructureTypeID != "2233" {
		t.Fatalf("unexpected customs office identity: %#v", event)
	}
	if event.AttackerAllianceID != "498125261" || event.AttackerCorporationID != "98774989" || event.AttackerCharacterID != "2119766668" {
		t.Fatalf("unexpected customs office aggressor: %#v", event)
	}
}

func TestClassifySovereigntyPayloads(t *testing.T) {
	tests := []struct {
		typeName  string
		text      string
		alertType string
	}{
		{"OwnershipTransferred", "solarSystemID: 30002904 structureID: 1043576893913 structureName: VFK-IV - Suslik North Home structureTypeID: 35834 newOwnerCorpID: 416584095 oldOwnerCorpID: 98599770", AlertStructureOwnership},
		{"StructureServicesOffline", "solarSystemID: 30002904 structureID: 1043576893913", AlertStructureOffline},
		{"SovCommandNodeEventStarted", "solarSystemID: 30001996 constellationID: 20000293", AlertSovCommandNodeEventStarted},
		{"SovStructureReinforced", "solarSystemID: 30002889 decloakTime: 134324704001705162", AlertSovStructureReinforced},
	}
	for _, test := range tests {
		t.Run(test.typeName, func(t *testing.T) {
			event, ok := Classify(&esi.Notification{ID: 1, Type: test.typeName, Timestamp: time.Now(), Text: test.text})
			if !ok || event.AlertType != test.alertType {
				t.Fatalf("unexpected classification: ok=%t event=%#v", ok, event)
			}
		})
	}
}

func TestClassifyExpandedNotificationPayloads(t *testing.T) {
	timestamp := time.Date(2026, time.September, 2, 2, 5, 0, 0, time.UTC)
	tests := []struct {
		name      string
		typeName  string
		text      string
		alertType string
		check     func(*testing.T, *Event)
	}{
		{
			name:      "mercenary den attack",
			typeName:  "MercenaryDenAttacked",
			alertType: AlertMercenaryDenAttacked,
			text: "aggressorAllianceName: Hostile Alliance\n" +
				"aggressorCharacterID: 2119766668\n" +
				"aggressorCorporationName: Hostile Corporation\n" +
				"itemID: &id001 1046813349750\n" +
				"planetID: 40126936\n" +
				"shieldPercentage: 94.5\n" +
				"armorPercentage: 100.0\n" +
				"hullPercentage: 100.0\n" +
				"solarsystemID: 30001988\n" +
				"typeID: 81826\n",
			check: assertMercenaryDenAttack,
		},
		{
			name:      "moon mining start",
			typeName:  "MoonminingExtractionStarted",
			alertType: AlertMoonminingExtractionStarted,
			text: "autoTime: 134329506560000000\nreadyTime: 134329506560000000\nmoonID: 40126937\n" +
				"solarSystemID: 30001988\nstartedBy: 2119766668\nstructureID: 1046813349750\nstructureTypeID: 81826\n",
			check: assertMoonMiningStart,
		},
		{
			name:      "self destruct",
			typeName:  "SovStructureSelfDestructRequested",
			alertType: AlertSovStructureSelfDestructRequested,
			text:      "charID: 2119766668 destructTime: 134329506560000000 solarSystemID: 30001988 structureTypeID: 35834",
			check:     assertSelfDestruct,
		},
		{
			name:      "bulk reinforcement schedule",
			typeName:  "StructuresReinforcementChanged",
			alertType: AlertStructuresReinforcementChanged,
			text:      "allStructureInfo: - - 1050629404880 - ZJET-E - VI - 13 - 81826 - - 1052657361104 - EL8-4Q - 4-1 - 81826 hour: 3 numStructures: 12 weekday: 5",
			check:     assertReinforcementSchedule,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, ok := Classify(&esi.Notification{ID: 10, Type: test.typeName, Timestamp: timestamp, Text: test.text})
			if !ok || event.AlertType != test.alertType {
				t.Fatalf("unexpected classification: ok=%t event=%#v", ok, event)
			}
			test.check(t, &event)
		})
	}
}

func TestClassifyEveryExpandedNotificationType(t *testing.T) {
	tests := map[string]string{
		"StructureOnline":                           AlertStructureOnline,
		"StructureWentLowPower":                     AlertStructureWentLowPower,
		"StructureWentHighPower":                    AlertStructureWentHighPower,
		"StructuresReinforcementChanged":            AlertStructuresReinforcementChanged,
		"StructureLowReagentsAlert":                 AlertStructureLowReagents,
		"StructureNoReagentsAlert":                  AlertStructureNoReagents,
		"StructureLostShields":                      AlertStructureLostShields,
		"StructureLostArmor":                        AlertStructureLostArmor,
		"StructureImpendingAbandonmentAssetsAtRisk": AlertStructureImpendingAbandonment,
		"SkyhookOnline":                             AlertSkyhookOnline,
		"SkyhookDeployed":                           AlertSkyhookDeployed,
		"MercenaryDenReinforced":                    AlertMercenaryDenReinforced,
		"MercenaryDenAttacked":                      AlertMercenaryDenAttacked,
		"MercenaryDenNewMTO":                        AlertMercenaryDenNewMTO,
		"MoonminingExtractionStarted":               AlertMoonminingExtractionStarted,
		"MoonminingExtractionCancelled":             AlertMoonminingExtractionCancelled,
		"MoonminingExtractionFinished":              AlertMoonminingExtractionFinished,
		"MoonminingLaserFired":                      AlertMoonminingLaserFired,
		"MoonminingAutomaticFracture":               AlertMoonminingAutomaticFracture,
		"SovStructureSelfDestructRequested":         AlertSovStructureSelfDestructRequested,
		"SovStructureSelfDestructCancel":            AlertSovStructureSelfDestructCancel,
		"SovStructureSelfDestructFinished":          AlertSovStructureSelfDestructFinished,
		"StationServiceEnabled":                     AlertStationServiceEnabled,
		"StationServiceDisabled":                    AlertStationServiceDisabled,
	}
	for typeName, wantAlertType := range tests {
		t.Run(typeName, func(t *testing.T) {
			event, ok := Classify(&esi.Notification{Type: typeName})
			if !ok || event.AlertType != wantAlertType {
				t.Fatalf("classification = ok=%t, alert_type=%q, want %q", ok, event.AlertType, wantAlertType)
			}
		})
	}
}

func TestClassifyEveryNotificationDefinition(t *testing.T) {
	for typeName, definition := range alertTypes {
		t.Run(typeName, func(t *testing.T) {
			event, ok := Classify(&esi.Notification{ID: 1, Type: typeName})
			if !ok {
				t.Fatalf("raw notification type %q did not classify", typeName)
			}
			if event.NotificationType != typeName {
				t.Fatalf("notification type = %q, want %q", event.NotificationType, typeName)
			}
			if event.AlertType != definition.leaf {
				t.Fatalf("canonical alert type = %q, want %q", event.AlertType, definition.leaf)
			}
			if !AlertTypeInGroup(event.AlertType, definition.group) {
				t.Fatalf("canonical alert type %q is not in group %q", event.AlertType, definition.group)
			}
		})
	}
}

func assertMercenaryDenAttack(t *testing.T, event *Event) {
	t.Helper()
	if event == nil {
		t.Fatal("event is nil")
	}
	if len(event.StructureIDs) != 1 || event.StructureIDs[0] != "1046813349750" || event.SystemID != "30001988" || event.StructureTypeID != "81826" {
		t.Fatalf("unexpected mercenary den identity: %#v", event)
	}
	if event.AttackerCharacterID != "2119766668" || event.AttackerCorporationName != "Hostile Corporation" || event.AttackerAllianceName != "Hostile Alliance" {
		t.Fatalf("unexpected mercenary den attacker: %#v", event)
	}
	if event.ShieldPercentage == nil || *event.ShieldPercentage != 94.5 {
		t.Fatalf("unexpected mercenary den shield value: %#v", event.ShieldPercentage)
	}
}

func assertMoonMiningStart(t *testing.T, event *Event) {
	t.Helper()
	if event == nil {
		t.Fatal("event is nil")
	}
	if event.ActorCharacterID != "2119766668" || event.MoonID != "40126937" || len(event.StructureIDs) != 1 || event.StructureIDs[0] != "1046813349750" {
		t.Fatalf("unexpected moon mining metadata: %#v", event)
	}
	if event.ReadyAt.IsZero() || event.AutoAt.IsZero() {
		t.Fatalf("expected moon mining times: %#v", event)
	}
}

func assertSelfDestruct(t *testing.T, event *Event) {
	t.Helper()
	if event == nil {
		t.Fatal("event is nil")
	}
	if event.ActorCharacterID != "2119766668" || event.DestructAt.IsZero() {
		t.Fatalf("unexpected self destruct metadata: %#v", event)
	}
}

func assertReinforcementSchedule(t *testing.T, event *Event) {
	t.Helper()
	if event == nil {
		t.Fatal("event is nil")
	}
	if event.ReinforcementHour == nil || *event.ReinforcementHour != 3 || event.ReinforcementWeekday == nil || *event.ReinforcementWeekday != 5 || event.ReinforcedStructureCount == nil || *event.ReinforcedStructureCount != 12 {
		t.Fatalf("unexpected reinforcement schedule: %#v", event)
	}
	if len(event.StructureIDs) != 2 || event.StructureIDs[0] != "1050629404880" || event.StructureIDs[1] != "1052657361104" {
		t.Fatalf("unexpected bulk structure IDs: %#v", event.StructureIDs)
	}
}

func TestClassifyDoesNotAddExcludedNotificationTypes(t *testing.T) {
	for _, typeName := range []string{
		"CorpStructLostMsg",
		"SkyhookLost",
		"SovStationEnteredFreeport",
		"SovereigntyTCUDamageMsg",
		"SovereigntySBUDamageMsg",
		"SovereigntyIHDamageMsg",
		"StructureItemsMovedToSafety",
		"StructureItemsDelivered",
		"StructureCourierContractChanged",
		"StructuresJobsPaused",
		"StructuresJobsCancelled",
		"WarDeclared",
	} {
		t.Run(typeName, func(t *testing.T) {
			if _, ok := Classify(&esi.Notification{Type: typeName}); ok {
				t.Fatalf("excluded notification type %q classified", typeName)
			}
		})
	}
}

func TestClassifyCapsStructureIDsFromMalformedPayload(t *testing.T) {
	parts := make([]string, 0, maxStructureIDs+4)
	for index := range maxStructureIDs + 4 {
		parts = append(parts, "structure_id: "+strconv.FormatInt(int64(100000000+index), 10))
	}
	event, ok := Classify(&esi.Notification{ID: 5, Type: "StructureUnderAttack", Text: strings.Join(parts, " ")})
	if !ok {
		t.Fatal("expected structure notification to classify")
	}
	if len(event.StructureIDs) != maxStructureIDs {
		t.Fatalf("structure ID count = %d, want %d", len(event.StructureIDs), maxStructureIDs)
	}
}

func TestClassifyUsesAllianceSenderForSovereigntyEvents(t *testing.T) {
	event, ok := Classify(&esi.Notification{
		ID:         4,
		Type:       "SovCommandNodeEventStarted",
		SenderID:   498125261,
		SenderType: "alliance",
		Text:       "campaignEventType: 2 constellationID: 20000293 solarSystemID: 30001996",
	})
	if !ok {
		t.Fatal("expected command-node notification to classify")
	}
	if event.SenderID != 498125261 || event.SenderType != "alliance" {
		t.Fatalf("expected alliance sender context to be preserved, got %#v", event)
	}
}
