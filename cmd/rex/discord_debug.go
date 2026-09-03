package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/btnmasher/rex/internal/alerts"
	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/config"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/esi"
	"github.com/btnmasher/rex/internal/logging"
	"github.com/btnmasher/rex/internal/notifications"
)

const (
	debugHTTPTimeout        = 20 * time.Second
	syntheticCorporationID  = ownerCorporationID
	syntheticCharacterID    = int64(90000001)
	firstNotificationID     = int64(900000001)
	structureID             = "1030000000001"
	structureTypeID         = "81826"
	customsOfficeTypeID     = "2233"
	skyhookStructureID      = "1046813349750"
	skyhookTypeID           = "81080"
	planetID                = "40126936"
	planetName              = "Synthetic Planet IV"
	solarSystemID           = "30000142"
	ownerCorporationID      = "98790350"
	ownerCorporationName    = "Unprofitable Ventures Inc."
	newOwnerCorporationID   = "416584095"
	newOwnerCorporationName = "Fourth District Sentinels"
	oldOwnerCorporationID   = "98599770"
	oldOwnerCorporationName = "Former Structure Owner"
	allianceID              = "99000001"
	allianceName            = "The Synthetic Alliance"
	attackerID              = "2119766668"
	attackerCorporationID   = "98774989"
	attackerAllianceID      = "498125261"
	structureTimeLeft       = "2591993262378"
)

func runDiscordDebug(ctx context.Context) error {
	if ctx == nil {
		return errors.New("discord debug context is required")
	}
	alertConfig, err := config.LoadAlertConfig()
	if err != nil {
		return err
	}
	if len(alertConfig.AlertDestinations) == 0 {
		return errors.New("no alert destinations are configured")
	}

	logger := logging.New("DEBUG", alertConfig.LogPretty)
	delivery := discord.NewWebhookDelivery(
		&http.Client{Timeout: debugHTTPTimeout},
		discord.WithLogger(logger),
	)
	destinations := make([]alerts.Destination, 0, len(alertConfig.AlertDestinations))
	for i := range alertConfig.AlertDestinations {
		destination := &alertConfig.AlertDestinations[i]
		destinations = append(destinations, alerts.Destination{
			ID:                      destination.Name,
			WebhookURLs:             destination.WebhookURLs,
			AlertTypes:              destination.AlertTypes,
			ExcludeAlertTypes:       destination.ExcludeAlertTypes,
			ExcludeStructureTypeIDs: destination.ExcludeStructureTypeIDs,
		})
	}
	alertService, err := alerts.NewService(debugDatabase{}, delivery, &alerts.Config{
		Destinations:            destinations,
		OverrideSenderName:      alertConfig.DiscordOverrideSenderName,
		OverrideSenderAvatarURL: alertConfig.DiscordOverrideSenderAvatarURL,
		ShowEntityIDs:           alertConfig.DiscordShowEntityIDs,
		LogPayloads:             alertConfig.LogPayloads,
		Logger:                  logger,
	})
	if err != nil {
		return err
	}

	events, err := syntheticEvents()
	if err != nil {
		return err
	}
	corporation := authnextdb.Corporation{
		ID:     syntheticCorporationID,
		Name:   "Synthetic Corporation",
		Ticker: "SYNTH",
	}
	for i := range events {
		event := &events[i]
		logger.Info("sending synthetic Discord alert", "alert_type", event.AlertType, "notification_id", event.NotificationID)
		if err := alertService.Deliver(ctx, &alerts.DeliveryRequest{
			Corporation: corporation,
			Event:       event,
		}); err != nil {
			return fmt.Errorf("deliver synthetic %s alert: %w", event.AlertType, err)
		}
	}
	logger.Info("synthetic Discord alerts delivered", "count", len(events))
	return nil
}

func syntheticEvents() ([]notifications.Event, error) {
	now := time.Now().UTC()
	fixtures := []struct {
		typeName string
		text     string
	}{
		{"StructureUnderAttack", structureText("<b>Keepstar under attack</b><br>Your Keepstar <i>Fort Gorgon</i> in Jita is under attack.")},
		{"StructureDestroyed", structureText("<b>Structure destroyed</b><br>Your Keepstar <i>Fort Gorgon</i> in Jita has been destroyed.")},
		{"StructureAnchoring", timedStructureText("<b>Structure anchoring</b><br>Your structure <i>Fort Gorgon</i> has begun anchoring in Jita.")},
		{"StructureUnanchoring", timedStructureText("<b>Structure unanchoring</b><br>Your structure <i>Fort Gorgon</i> has begun unanchoring in Jita.")},
		{"StructureVulnerable", timedStructureText("<b>Structure vulnerable</b><br>Your structure <i>Fort Gorgon</i> is now vulnerable in Jita.")},
		{"StructureReinforced", timedStructureText("<b>Structure reinforced</b><br>Your structure <i>Fort Gorgon</i> has entered reinforced mode in Jita.")},
		{"StructureServicesOffline", structureText("<b>Structure services offline</b><br>Services at <i>Fort Gorgon</i> are offline in Jita.")},
		{"StructureOnline", structureText("<b>Structure online</b><br>Your structure <i>Fort Gorgon</i> is now online in Jita.")},
		{"StructureWentLowPower", structureText("<b>Structure low power</b><br>Your structure <i>Fort Gorgon</i> has entered low power mode in Jita.")},
		{"StructureWentHighPower", structureText("<b>Structure high power</b><br>Your structure <i>Fort Gorgon</i> has entered high power mode in Jita.")},
		{"StructuresReinforcementChanged", timedStructureText("<b>Reinforcement schedule changed</b><br>The reinforcement schedule for <i>Fort Gorgon</i> in Jita has changed.")},
		{"StructureFuelAlert", structureText("<b>Structure fuel alert</b><br>Your structure <i>Fort Gorgon</i> is low on fuel and may enter low power.")},
		{"StructureLowReagentsAlert", structureText("<b>Structure low on reagents</b><br>Your structure <i>Fort Gorgon</i> is running low on reagents.")},
		{"StructureNoReagentsAlert", structureText("<b>Structure out of reagents</b><br>Your structure <i>Fort Gorgon</i> is out of reagents.")},
		{"StructureLostShields", structureText("<b>Structure lost shields</b><br>Your structure <i>Fort Gorgon</i> has lost its shields.")},
		{"StructureLostArmor", structureText("<b>Structure lost armor</b><br>Your structure <i>Fort Gorgon</i> has lost its armor.")},
		{"StructureImpendingAbandonmentAssetsAtRisk", structureText("<b>Structure abandonment risk</b><br>Assets at your structure <i>Fort Gorgon</i> are at risk of abandonment.")},
		{"EntosisCaptureStarted", systemText("<b>Entosis link detected</b><br>An entosis link has been activated against the sovereignty of Jita.")},
		{"EntosisCaptureFinished", systemText("<b>Entosis event complete</b><br>The entosis event affecting Jita has completed.")},
		{"EntosisCaptureNodesReinforced", systemText("<b>Entosis nodes reinforced</b><br>Entosis nodes for the sovereignty of Jita have entered reinforced mode.")},
		{"ESSMainBankLink", systemText("<b>Main bank link detected</b><br>A player has linked to the main bank of the Encounter Surveillance System in Jita.")},
		{"ESSReserveBankLink", systemText("<b>Reserve bank link detected</b><br>A player has linked to the reserve bank of the Encounter Surveillance System in Jita.")},
		{"SkyhookUnderAttack", skyhookUnderAttackText()},
		{"SkyhookLostShields", skyhookLostShieldsText()},
		{"SkyhookDestroyed", skyhookDestroyedText()},
		{"SkyhookOnline", structureText("<b>Skyhook online</b><br>The skyhook in Jita is online.")},
		{"SkyhookDeployed", structureText("<b>Skyhook deployed</b><br>A skyhook has been deployed in Jita.")},
		{"TowerAlertMsg", towerAlertText()},
		{"TowerResourceAlertMsg", towerResourceAlertText()},
		{"OrbitalAttacked", orbitalAttackedText()},
		{"OrbitalReinforced", orbitalReinforcedText()},
		{"MercenaryDenReinforced", structureText("<b>Mercenary Den reinforced</b><br>The Mercenary Den in Jita has entered reinforced mode.")},
		{"MercenaryDenAttacked", structureText("<b>Mercenary Den attacked</b><br>The Mercenary Den in Jita is under attack.")},
		{"MercenaryDenNewMTO", structureText("<b>Mercenary Den tactical operation</b><br>A new tactical operation is available in Jita.")},
		{"MoonminingExtractionStarted", systemText("<b>Moon extraction started</b><br>A moon mining extraction has started in Jita.")},
		{"MoonminingExtractionCancelled", systemText("<b>Moon extraction canceled</b><br>A moon mining extraction was canceled in Jita.")},
		{"MoonminingExtractionFinished", systemText("<b>Moon extraction finished</b><br>A moon mining extraction finished in Jita.")},
		{"MoonminingLaserFired", systemText("<b>Moon mining laser fired</b><br>A moon mining laser fired in Jita.")},
		{"MoonminingAutomaticFracture", systemText("<b>Moon automatic fracture</b><br>An automatic moon fracture occurred in Jita.")},
		{"OwnershipTransferred", "charID: 2113551184 newOwnerCorpID: 416584095 oldOwnerCorpID: 98599770 solarSystemID: 30002904 structureID: 1043576893913 structureName: VFK-IV - Suslik North Home structureTypeID: 35834"},
		{"SovStationEnteredReinforce", timedStructureText("<b>Sovereignty Hub reinforced</b><br>The Sovereignty Hub in Jita has entered reinforced mode.")},
		{"SovStationExitedReinforce", structureText("<b>Sovereignty Hub exited reinforce</b><br>The Sovereignty Hub in Jita has exited reinforced mode.")},
		{"SovStructureReinforced", structureText("<b>Sovereignty Hub reinforced</b><br>A Sovereignty Hub in Jita has been reinforced.")},
		{"SovStructureDestroyed", structureText("<b>Sovereignty Hub destroyed</b><br>A Sovereignty Hub in Jita has been destroyed.")},
		{"SovStructureSelfDestructRequested", timedStructureText("<b>Sovereignty Hub self-destruct requested</b><br>A Sovereignty Hub in Jita has been scheduled for self-destruct.")},
		{"SovStructureSelfDestructCancel", structureText("<b>Sovereignty Hub self-destruct canceled</b><br>The self-destruct of a Sovereignty Hub in Jita was canceled.")},
		{"SovStructureSelfDestructFinished", structureText("<b>Sovereignty Hub self-destruct finished</b><br>A Sovereignty Hub in Jita completed self-destruct.")},
		{"SovCommandNodeEventStarted", "campaignEventType: 2 constellationID: 20000293 solarSystemID: 30001996"},
		{"SovAllClaimAquiredMsg", allianceText("<b>Sovereignty claimed</b><br>Your alliance has claimed sovereignty of Jita.")},
		{"SovAllClaimLostMsg", allianceText("<b>Sovereignty lost</b><br>Your alliance has lost sovereignty of Jita.")},
		{"SovereigntyClaimed", allianceText("<b>Sovereignty claimed</b><br>The alliance has claimed sovereignty of Jita.")},
		{"SovereigntyLost", allianceText("<b>Sovereignty lost</b><br>The alliance has lost sovereignty of Jita.")},
		{"StationServiceEnabled", structureText("<b>Station service enabled</b><br>A station service at <i>Fort Gorgon</i> in Jita was enabled.")},
		{"StationServiceDisabled", structureText("<b>Station service disabled</b><br>A station service at <i>Fort Gorgon</i> in Jita was disabled.")},
	}
	notificationsToClassify := make([]esi.Notification, 0, len(fixtures))
	for i, fixture := range fixtures {
		senderID := syntheticCharacterID
		senderType := "character"
		if fixture.typeName == "SovCommandNodeEventStarted" {
			senderID = 498125261
			senderType = "alliance"
		}
		notificationsToClassify = append(notificationsToClassify, esi.Notification{
			ID:         firstNotificationID + int64(i),
			Type:       fixture.typeName,
			SenderID:   senderID,
			SenderType: senderType,
			Timestamp:  now.Add(-time.Duration(i) * time.Second),
			Text:       fixture.text,
		})
	}
	events := make([]notifications.Event, 0, len(notificationsToClassify))
	for i := range notificationsToClassify {
		event, ok := notifications.Classify(&notificationsToClassify[i])
		if !ok {
			return nil, fmt.Errorf("synthetic notification %q was not classified", notificationsToClassify[i].Type)
		}
		events = append(events, event)
	}
	return events, nil
}

func structureText(message string) string {
	return structureTextWithTime(message, false)
}

func timedStructureText(message string) string {
	return structureTextWithTime(message, true)
}

func structureTextWithTime(message string, includeTimeLeft bool) string {
	text := message + " ownerCorpLinkData: - showinfo - 2 - " + ownerCorporationID +
		" ownerCorpName: " + ownerCorporationName +
		" solarsystemID: " + solarSystemID +
		" structureID: &id001 " + structureID +
		" structureShowInfoData: - showinfo - " + structureTypeID + " - *id001"
	if includeTimeLeft {
		text += " timeLeft: " + structureTimeLeft
	}
	return text
}

func systemText(message string) string {
	return message + " solarsystemID: " + solarSystemID
}

func allianceText(message string) string {
	return message + " allianceID: " + allianceID + " corpID: " + ownerCorporationID + " solarSystemID: " + solarSystemID
}

func skyhookUnderAttackText() string {
	return "allianceID: 99013797 allianceName: The Synthetic Alliance corpLinkData: - showinfo - 2 - 98774989 corpName: Fourth District Sentinels charID: 2119766668 charName: Synthetic Attacker itemID: &id001 " +
		skyhookStructureID + " planetID: " + planetID + " solarsystemID: " + solarSystemID +
		" shieldPercentage: 94.6 armorPercentage: 100.0 hullPercentage: 100.0 typeID: " + skyhookTypeID
}

func skyhookLostShieldsText() string {
	return "itemID: &id001 " + skyhookStructureID + " planetID: " + planetID + " solarsystemID: " + solarSystemID +
		" typeID: " + skyhookTypeID + " vulnerableTime: 9000000000"
}

func skyhookDestroyedText() string {
	return "itemID: &id001 " + skyhookStructureID + " planetID: " + planetID + " solarsystemID: " + solarSystemID +
		" typeID: " + skyhookTypeID
}

func towerAlertText() string {
	return "<b>Starbase under attack</b><br>A starbase is under attack in Jita." +
		" aggressorAllianceID: " + attackerAllianceID +
		" aggressorCorpID: " + attackerCorporationID +
		" aggressorID: " + attackerID +
		" armorValue: 75.0 hullValue: 100.0 moonID: 40126937" +
		" shieldValue: 50.0 solarSystemID: " + solarSystemID + " typeID: 35834"
}

func towerResourceAlertText() string {
	return "<b>Starbase resource alert</b><br>A starbase needs fuel in Jita." +
		" allianceID: " + allianceID + " corpID: " + ownerCorporationID +
		" moonID: 40126937 solarSystemID: " + solarSystemID + " typeID: 35834" +
		" wants: - quantity: 100 typeID: 4247 - quantity: 20 typeID: 4246"
}

func orbitalAttackedText() string {
	return "aggressorAllianceID: " + attackerAllianceID +
		" aggressorCorpID: " + attackerCorporationID +
		" aggressorID: " + attackerID +
		" planetID: " + planetID + " planetTypeID: 2015 shieldLevel: 0.945" +
		" solarSystemID: " + solarSystemID + " typeID: " + customsOfficeTypeID
}

func orbitalReinforcedText() string {
	return "aggressorAllianceID: " + attackerAllianceID +
		" aggressorCorpID: " + attackerCorporationID +
		" aggressorID: " + attackerID +
		" planetID: " + planetID + " planetTypeID: 2015" +
		" reinforceExitTime: 134329506560000000" +
		" solarSystemID: " + solarSystemID + " typeID: " + customsOfficeTypeID
}

type debugDatabase struct{}

func (debugDatabase) GetStructuresByIDs(ctx context.Context, structureIDs []string) ([]authnextdb.Structure, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	structures := make([]authnextdb.Structure, 0, len(structureIDs))
	for _, id := range structureIDs {
		name := "Synthetic Keepstar"
		systemName := "Jita"
		structureSystemID := solarSystemID
		typeID := structureTypeID
		if id == "1043576893913" {
			name = "VFK-IV - Suslik North Home"
			systemName = "VFK-IV"
			structureSystemID = "30002904"
			typeID = "35834"
		}
		if id == skyhookStructureID {
			name = "Synthetic Skyhook"
			typeID = skyhookTypeID
		}
		structures = append(structures, authnextdb.Structure{
			ID:         id,
			Name:       &name,
			TypeID:     typeID,
			SystemID:   structureSystemID,
			SystemName: &systemName,
			PlanetID:   new(planetID),
			PlanetName: new(planetName),
		})
	}
	return structures, nil
}

func (debugDatabase) ResolveNames(ctx context.Context, kind string, ids []string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names := make(map[string]string, len(ids))
	for _, id := range ids {
		if name := debugUniverseName(kind, id); name != "" {
			names[id] = name
		}
	}
	return names, nil
}

func debugUniverseName(kind, id string) string {
	switch kind {
	case "solar_system":
		switch id {
		case solarSystemID:
			return "Jita"
		case "30002904":
			return "VFK-IV"
		case "30001996":
			return "Synthetic Command System"
		}
	case "alliance":
		switch id {
		case allianceID:
			return allianceName
		case "498125261":
			return "The Caldari Fourth District"
		}
	case "corporation":
		switch id {
		case newOwnerCorporationID:
			return newOwnerCorporationName
		case oldOwnerCorporationID:
			return oldOwnerCorporationName
		}
	}
	return ""
}
