// Package alerts resolves destinations and renders classified notification alerts.
package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/discord"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/notificationstate"
	"github.com/btnmasher/rex/internal/universe"
)

const (
	colorDanger                   = 0xD7263D
	colorWarning                  = 0xF1C40F
	colorSuccess                  = 0x2ECC71
	colorInformational            = 0x2D6CDF
	maxTextSize                   = 3800
	maxStructureSummarySize       = 1024
	maxIntegrityValues            = 3
	maxAttackerParts              = 3
	maxOwnershipParts             = 2
	maxTimerFields                = 4
	maxActivityFields             = 4
	maxScheduleValues             = 3
	maxLoggedPayloadBytes         = 128 << 10
	reinforcementWeekdayUnchanged = 255
	discordTimestampFull          = "F"
	discordTimestampRelative      = "R"
	allianceLogoURL               = "https://images.evetech.net/alliances/%s/logo?size=64"
	corporationLogoURL            = "https://images.evetech.net/corporations/%s/logo?size=64"
	eveWhoCharacterURL            = "https://evewho.com/character/%s"
	eveWhoCorporationURL          = "https://evewho.com/corporation/%s"
	eveWhoAllianceURL             = "https://evewho.com/alliance/%s"
	dotlanSystemURL               = "https://evemaps.dotlan.net/system/%s"
	dotlanRegionURL               = "https://evemaps.dotlan.net/region/%s"
)

var eventDescriptionTemplates = map[string]string{
	"StructureUnderAttack":                      "A structure is under attack in %s.",
	"StructureDestroyed":                        "A structure has been destroyed in %s.",
	"StructureAnchoring":                        "A structure has started anchoring in %s.",
	"StructureUnanchoring":                      "A structure has started unanchoring in %s.",
	"StructureVulnerable":                       "A structure is vulnerable in %s.",
	"StructureReinforced":                       "A structure has entered reinforced mode in %s.",
	"StructureOnline":                           "A structure has come online in %s.",
	"StructureWentLowPower":                     "A structure has entered low power mode in %s.",
	"StructureWentHighPower":                    "A structure has entered full power mode in %s.",
	"StructuresReinforcementChanged":            "The reinforcement schedule changed for structures in %s.",
	"StructureServicesOffline":                  "Services on a structure are offline in %s.",
	"StructureFuelAlert":                        "A structure has a fuel warning in %s.",
	"StructureLowReagentsAlert":                 "A structure has low reagents in %s.",
	"StructureNoReagentsAlert":                  "A structure has no reagents in %s.",
	"StructureLostShields":                      "A structure has lost its shields in %s.",
	"StructureLostArmor":                        "A structure has lost its armor in %s.",
	"StructureImpendingAbandonmentAssetsAtRisk": "A structure's assets are at risk of abandonment in %s.",
	"EntosisCaptureStarted":                     "Sovereignty entosis capture has started in %s.",
	"EntosisCaptureFinished":                    "Sovereignty entosis capture has finished in %s.",
	"EntosisCaptureNodesReinforced":             "Sovereignty entosis nodes have entered reinforced mode in %s.",
	"ESSMainBankLink":                           "The ESS main bank has been linked in %s.",
	"ESSReserveBankLink":                        "The ESS reserve bank has been linked in %s.",
	"SkyhookLostShields":                        "A skyhook has lost its shields and entered reinforced mode in %s.",
	"SkyhookDestroyed":                          "A skyhook has been destroyed in %s.",
	"SkyhookOnline":                             "A skyhook has come online in %s.",
	"SkyhookDeployed":                           "A skyhook has been deployed in %s.",
	"MercenaryDenReinforced":                    "A mercenary den has entered reinforced mode in %s.",
	"MercenaryDenAttacked":                      "A mercenary den is under attack in %s.",
	"MercenaryDenNewMTO":                        "A mercenary den has received a new tactical operation in %s.",
	"MoonminingExtractionStarted":               "A moon mining extraction has started in %s.",
	"MoonminingExtractionCancelled":             "A moon mining extraction was canceled in %s.",
	"MoonminingExtractionFinished":              "A moon mining extraction has finished in %s.",
	"MoonminingLaserFired":                      "A moon mining laser fired in %s.",
	"MoonminingAutomaticFracture":               "A moon mining extraction fractured automatically in %s.",
	"OwnershipTransferred":                      "Structure ownership has transferred in %s.",
	"SovStationEnteredReinforce":                "A Sovereignty Hub has entered reinforced mode in %s.",
	"SovStationExitedReinforce":                 "A Sovereignty Hub has exited reinforced mode in %s.",
	"SovStructureReinforced":                    "A Sovereignty Hub has been reinforced in %s.",
	"SovStructureDestroyed":                     "A Sovereignty Hub has been destroyed in %s.",
	"SovStructureSelfDestructRequested":         "A Sovereignty Hub self-destruct has been requested in %s.",
	"SovStructureSelfDestructCancel":            "A Sovereignty Hub self-destruct was canceled in %s.",
	"SovStructureSelfDestructFinished":          "A Sovereignty Hub self-destruct has finished in %s.",
	"SovAllClaimAquiredMsg":                     "Sovereignty has been claimed in %s.",
	"SovereigntyClaimed":                        "Sovereignty has been claimed in %s.",
	"SovAllClaimLostMsg":                        "Sovereignty has been lost in %s.",
	"SovereigntyLost":                           "Sovereignty has been lost in %s.",
	"StationServiceEnabled":                     "A station service has been enabled in %s.",
	"StationServiceDisabled":                    "A station service has been disabled in %s.",
	"TowerAlertMsg":                             "A starbase is under attack in %s.",
	"TowerResourceAlertMsg":                     "A starbase has reported a resource shortage in %s.",
	"OrbitalAttacked":                           "A customs office is under attack in %s.",
	"OrbitalReinforced":                         "A customs office has entered reinforced mode in %s.",
}

var eventFieldRenderers = [...]func(*eventView) []discord.Field{
	corporationFields,
	ownershipFields,
	whenFields,
	attackerFields,
	actorFields,
	allianceFields,
	systemFields,
	regionFields,
	planetFields,
	structureFields,
	structureTypeFields,
	integrityFields,
	resourceFields,
	timerFields,
	activityFields,
	decloakFields,
}

// Service resolves auth-next configuration and delivers notification embeds.
type Service struct {
	database                Database
	delivery                discord.Delivery
	destinations            []Destination
	overrideSenderName      string
	overrideSenderAvatarURL string
	showEntityIDs           bool
	logPayloads             bool
	logger                  *slog.Logger
	resolver                universe.Resolver
	history                 notificationstate.AlertHistory
}

// Destination is a named Discord destination and its canonical alert selector mapping.
type Destination struct {
	ID                      string
	WebhookURLs             []string
	AlertTypes              []string
	ExcludeAlertTypes       []string
	ExcludeStructureTypeIDs []string
	IncludeCorporationIDs   []string
	ExcludeCorporationIDs   []string
	routing                 alertRouting
	corporations            corporationFilter
}

type alertRouting struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

type corporationFilter struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

// Config controls local alert routing and optional Discord webhook identity overrides.
type Config struct {
	Destinations            []Destination
	OverrideSenderName      string
	OverrideSenderAvatarURL string
	ShowEntityIDs           bool
	LogPayloads             bool
	Logger                  *slog.Logger
	UniverseResolver        universe.Resolver
	History                 notificationstate.AlertHistory
}

// DeliveryRequest identifies one event and optionally limits delivery to the
// destinations that failed in a previous attempt.
type DeliveryRequest struct {
	Corporation         authnextdb.Corporation
	CharacterID         string
	RawNotificationJSON []byte
	Event               *notifications.Event
	DestinationIDs      []string
}

// DeliveryError reports the destinations that did not accept an alert.
type DeliveryError struct {
	DestinationIDs []string
	Err            error
}

// Error returns the aggregate destination delivery failure.
func (e *DeliveryError) Error() string {
	if e == nil || e.Err == nil {
		return "alert delivery failed"
	}
	return e.Err.Error()
}

// Unwrap exposes the aggregate delivery failure to errors.Is and errors.As.
func (e *DeliveryError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Database is the persistence subset needed to resolve structure names.
type Database interface {
	GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error)
}

// NameResolver optionally resolves universe names for richer alert fields.
type NameResolver interface {
	ResolveNames(context.Context, string, []string) (map[string]string, error)
}

// NewService creates the alert delivery service. The structure database may be
// nil; universe enrichment remains best-effort through the configured resolver.
func NewService(database Database, delivery discord.Delivery, config *Config) (*Service, error) {
	if delivery == nil || config == nil {
		return nil, errors.New("alert service dependencies are required")
	}
	destinations, err := cloneDestinations(config.Destinations)
	if err != nil {
		return nil, err
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		database:                database,
		delivery:                delivery,
		destinations:            destinations,
		overrideSenderName:      config.OverrideSenderName,
		overrideSenderAvatarURL: config.OverrideSenderAvatarURL,
		showEntityIDs:           config.ShowEntityIDs,
		logPayloads:             config.LogPayloads,
		logger:                  logger,
		resolver:                config.UniverseResolver,
		history:                 config.History,
	}, nil
}

func cloneDestinations(values []Destination) ([]Destination, error) {
	clones := make([]Destination, len(values))
	for i := range values {
		clones[i] = values[i]
		clones[i].AlertTypes = append([]string(nil), values[i].AlertTypes...)
		clones[i].ExcludeAlertTypes = append([]string(nil), values[i].ExcludeAlertTypes...)
		clones[i].ExcludeStructureTypeIDs = append([]string(nil), values[i].ExcludeStructureTypeIDs...)
		clones[i].IncludeCorporationIDs = append([]string(nil), values[i].IncludeCorporationIDs...)
		clones[i].ExcludeCorporationIDs = append([]string(nil), values[i].ExcludeCorporationIDs...)
		clones[i].WebhookURLs = append([]string(nil), values[i].WebhookURLs...)
		routing, err := compileAlertRouting(clones[i].AlertTypes, clones[i].ExcludeAlertTypes)
		if err != nil {
			return nil, fmt.Errorf("destination %q: %w", clones[i].ID, err)
		}
		clones[i].routing = routing
		clones[i].corporations = compileCorporationFilter(clones[i].IncludeCorporationIDs, clones[i].ExcludeCorporationIDs)
	}
	return clones, nil
}

// Deliver resolves configured destinations and sends an alert event.
func (s *Service) Deliver(ctx context.Context, request *DeliveryRequest) error {
	if request == nil || request.Event == nil {
		return errors.New("alert event is required")
	}
	if len(s.destinations) == 0 {
		return nil
	}
	requested := requestedDestinationIDs(request)
	matching := s.matchingDestinations(requested, request.Corporation.ID, request.Event.AlertType)
	if len(matching) == 0 {
		s.logger.Debug("alert has no configured destinations",
			"notification_id", request.Event.NotificationID,
			"alert_type", request.Event.AlertType,
		)
		return nil
	}
	view, err := s.buildEventView(ctx, request)
	if err != nil {
		return err
	}
	matching = s.filterDestinations(request, view, matching)
	if len(matching) == 0 {
		return nil
	}
	targets := flattenWebhookTargets(matching, requested)
	if len(targets) == 0 {
		return nil
	}
	s.logDestinationsResolved(request, len(targets))
	return s.deliverTargets(ctx, view, targets)
}

func requestedDestinationIDs(request *DeliveryRequest) map[string]struct{} {
	requested := make(map[string]struct{}, len(request.DestinationIDs))
	for _, destinationID := range request.DestinationIDs {
		requested[destinationID] = struct{}{}
	}
	return requested
}

func (s *Service) matchingDestinations(requested map[string]struct{}, corporationID, alertType string) []Destination {
	matching := make([]Destination, 0, len(s.destinations))
	for i := range s.destinations {
		destination := &s.destinations[i]
		if destinationSelected(requested, destination) && destination.corporations.supports(corporationID) && destination.routing.supports(alertType) {
			matching = append(matching, *destination)
		}
	}
	return matching
}

func (s *Service) deliverTargets(ctx context.Context, view *eventView, targets []webhookTarget) error {
	var errs []error
	failedIDs := make([]string, 0)
	for i := range targets {
		if err := s.deliverTarget(ctx, &targets[i], view); err != nil {
			errs = append(errs, err)
			failedIDs = append(failedIDs, targets[i].ID)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &DeliveryError{DestinationIDs: failedIDs, Err: errors.Join(errs...)}
}

func (s *Service) filterDestinations(request *DeliveryRequest, view *eventView, matching []Destination) []Destination {
	filtered := matching[:0]
	for i := range matching {
		if destinationExcludesStructureType(&matching[i], view) {
			s.logger.Debug("alert destination filtered by structure type",
				"destination_id", matching[i].ID,
				"notification_id", request.Event.NotificationID,
				"structure_type_id", structureTypeID(view),
			)
			continue
		}
		filtered = append(filtered, matching[i])
	}
	return filtered
}

func (s *Service) buildEventView(ctx context.Context, request *DeliveryRequest) (*eventView, error) {
	if request == nil || request.Event == nil {
		return nil, errors.New("alert event is required")
	}
	event := *request.Event
	var structures []authnextdb.Structure
	if s.database != nil {
		var err error
		structures, err = s.database.GetStructuresByIDs(ctx, event.StructureIDs)
		if err != nil {
			s.logger.Warn("alert structure enrichment failed", "notification_id", event.NotificationID, "err", err)
			structures = nil
		}
	}
	s.enrichStructures(ctx, structures)
	event.StructureTypeID = structureTypeIDForEvent(&event, structures)
	structureTypeName := s.resolveStructureTypeName(ctx, event.StructureTypeID, structures)
	structureTypeNames := s.resolveStructureTypeNames(ctx, structures)
	location := s.resolveLocation(ctx, event.SystemID)
	if event.AllianceID == "" && location.AllianceID != "" {
		event.AllianceID = location.AllianceID
		event.AllianceName = location.AllianceName
	}
	if event.AllianceID == "" && strings.EqualFold(event.SenderType, "alliance") && event.SenderID > 0 {
		event.AllianceID = strconv.FormatInt(event.SenderID, 10)
	}
	allianceName, allianceIconURL, err := s.resolveAlliance(ctx, &event)
	if err != nil {
		s.logger.Warn("alert alliance enrichment failed", "alliance_id", event.AllianceID, "err", err)
		allianceName = event.AllianceName
		allianceIconURL = ""
	}
	s.enrichAttacker(ctx, &event)
	s.enrichActor(ctx, &event)
	s.enrichOwnership(ctx, &event)
	planetName := s.resolveCelestialName(ctx, "planet", event.PlanetID)
	moonName := s.resolveCelestialName(ctx, "moon", event.MoonID)
	s.logger.Debug("alert structures enriched",
		"notification_id", event.NotificationID,
		"structure_count", len(structures),
	)
	// The ESI owner corporation is authoritative for structure notifications.
	// The polling corporation remains a display fallback when owner metadata or
	// a tracked structure record is unavailable.
	corporationOwned := len(event.StructureIDs) > 0 || event.OwnerCorporationID != "" || notifications.AlertTypeInGroup(event.AlertType, notifications.AlertStarbase) || notifications.AlertTypeInGroup(event.AlertType, notifications.AlertCustomsOffices)
	view := &eventView{
		CorporationID:            request.Corporation.ID,
		CorporationName:          request.Corporation.Name,
		CorporationTicker:        request.Corporation.Ticker,
		CorporationOwned:         corporationOwned,
		SystemName:               location.Name,
		RegionID:                 location.RegionID,
		RegionName:               location.RegionName,
		AllianceName:             allianceName,
		AllianceIconURL:          allianceIconURL,
		ShowEntityIDs:            s.showEntityIDs,
		PlanetName:               planetName,
		MoonName:                 moonName,
		Event:                    event,
		Structures:               structures,
		CharacterID:              request.CharacterID,
		RawNotificationJSON:      append([]byte(nil), request.RawNotificationJSON...),
		PollingCorporationID:     request.Corporation.ID,
		PollingCorporationName:   request.Corporation.Name,
		PollingCorporationTicker: request.Corporation.Ticker,
		StructureTypeName:        structureTypeName,
		StructureTypeNames:       structureTypeNames,
	}
	if event.OwnerCorporationID != "" {
		view.CorporationID = event.OwnerCorporationID
		if event.OwnerCorporationID != request.Corporation.ID {
			view.CorporationTicker = ""
		}
	}
	if event.OwnerCorporationName != "" {
		view.CorporationName = event.OwnerCorporationName
	}
	return view, nil
}

func (s *Service) resolveCelestialName(ctx context.Context, kind, id string) string {
	if id == "" {
		return ""
	}
	if s.resolver == nil {
		return ""
	}
	entity, err := s.resolveCelestial(ctx, kind, id)
	if err != nil {
		s.logger.Warn("alert celestial enrichment failed", "kind", kind, "id", id, "err", err)
		return ""
	}
	if entity.Name != "" {
		return entity.Name
	}
	return ""
}

func (s *Service) resolveStructureTypeName(ctx context.Context, typeID string, structures []authnextdb.Structure) string {
	if typeID == "" {
		return ""
	}
	for index := range structures {
		structure := &structures[index]
		if structure.TypeID == typeID && structure.TypeName != nil && strings.TrimSpace(*structure.TypeName) != "" {
			return strings.TrimSpace(*structure.TypeName)
		}
	}
	if s.resolver != nil {
		entity, err := s.resolver.ResolveStructureType(ctx, typeID)
		if err != nil {
			s.logger.Warn("alert structure type enrichment failed", "type_id", typeID, "err", err)
			return ""
		}
		return entity.Name
	}
	name, err := s.resolveName(ctx, "type", typeID)
	if err != nil {
		s.logger.Warn("alert structure type enrichment failed", "type_id", typeID, "err", err)
		return ""
	}
	return name
}

func (s *Service) resolveStructureTypeNames(ctx context.Context, structures []authnextdb.Structure) map[string]string {
	names := make(map[string]string, len(structures))
	for index := range structures {
		typeID := strings.TrimSpace(structures[index].TypeID)
		if typeID == "" {
			continue
		}
		if _, resolved := names[typeID]; resolved {
			continue
		}
		name := optionalString(structures[index].TypeName)
		if name == "" {
			name = s.resolveStructureTypeName(ctx, typeID, nil)
		}
		if name != "" {
			names[typeID] = name
		}
	}
	return names
}

func (s *Service) resolveCelestial(ctx context.Context, kind, id string) (universe.Entity, error) {
	if s == nil || s.resolver == nil {
		return universe.Entity{}, errors.New("universe resolver is unavailable")
	}
	if kind == "planet" {
		return s.resolver.ResolvePlanet(ctx, id)
	}
	return s.resolver.ResolveMoon(ctx, id)
}

func (s *Service) resolveLocation(ctx context.Context, systemID string) universe.SolarSystem {
	location := universe.SolarSystem{ID: systemID}
	if systemID == "" {
		return location
	}
	if s.resolver != nil {
		resolved, err := s.resolver.ResolveSolarSystem(ctx, systemID)
		if err != nil {
			s.logger.Warn("alert solar-system enrichment failed", "system_id", systemID, "err", err)
		} else {
			location = resolved
		}
	}
	if location.Name == "" {
		name, err := s.resolveName(ctx, "solar_system", systemID)
		if err != nil {
			s.logger.Warn("alert solar-system name enrichment failed", "system_id", systemID, "err", err)
		} else {
			location.Name = name
		}
	}
	if location.RegionID != "" && location.RegionName == "" && s.resolver != nil {
		region, err := s.resolver.ResolveRegion(ctx, location.RegionID)
		if err != nil {
			s.logger.Warn("alert region enrichment failed", "region_id", location.RegionID, "err", err)
		} else {
			location.RegionName = region.Name
		}
	}
	return location
}

func (s *Service) resolveAlliance(ctx context.Context, event *notifications.Event) (allianceName, iconURL string, err error) {
	if event == nil || event.AllianceID == "" {
		return "", "", nil
	}
	allianceName = event.AllianceName
	if s.resolver == nil {
		return s.resolveAllianceDatabase(ctx, event.AllianceID, allianceName)
	}
	resolved, resolveErr := s.resolver.ResolveAlliance(ctx, event.AllianceID)
	if resolveErr != nil {
		s.logger.Warn("alert alliance enrichment failed", "alliance_id", event.AllianceID, "err", resolveErr)
		return s.resolveAllianceDatabase(ctx, event.AllianceID, allianceName)
	}
	if resolved.Name != "" {
		allianceName = resolved.Name
	}
	if allianceName == "" {
		return s.resolveAllianceDatabase(ctx, event.AllianceID, allianceName)
	}
	return allianceName, resolved.IconURL, nil
}

func (s *Service) resolveAllianceDatabase(ctx context.Context, allianceID, fallback string) (name, iconURL string, err error) {
	if fallback != "" {
		return fallback, "", nil
	}
	name, err = s.resolveName(ctx, "alliance", allianceID)
	if err != nil {
		s.logger.Warn("alert alliance database enrichment failed", "alliance_id", allianceID, "err", err)
		return fallback, "", nil
	}
	return name, "", nil
}

func (s *Service) enrichAttacker(ctx context.Context, event *notifications.Event) {
	if event == nil || s.resolver == nil || event.AttackerCharacterID == "" && event.AttackerCorporationID == "" {
		return
	}
	if event.AttackerCharacterName == "" && event.AttackerCharacterID != "" {
		character, err := s.resolver.ResolveCharacter(ctx, event.AttackerCharacterID)
		if err != nil {
			s.logger.Warn("alert attacker character enrichment failed", "character_id", event.AttackerCharacterID, "err", err)
		} else if character.Name != "" {
			event.AttackerCharacterName = character.Name
		}
	}
	if event.AttackerCorporationName == "" && event.AttackerCorporationID != "" {
		corporation, err := s.resolver.ResolveCorporation(ctx, event.AttackerCorporationID)
		if err != nil {
			s.logger.Warn("alert attacker corporation enrichment failed", "corporation_id", event.AttackerCorporationID, "err", err)
		} else if corporation.Name != "" {
			event.AttackerCorporationName = corporation.Name
		}
	}
	s.enrichAttackerAlliance(ctx, event)
}

func (s *Service) enrichAttackerAlliance(ctx context.Context, event *notifications.Event) {
	if event == nil || s.resolver == nil || event.AttackerAllianceName != "" || event.AttackerAllianceID == "" {
		return
	}
	alliance, err := s.resolver.ResolveAlliance(ctx, event.AttackerAllianceID)
	if err != nil {
		s.logger.Warn("alert attacker alliance enrichment failed", "alliance_id", event.AttackerAllianceID, "err", err)
		return
	}
	if alliance.Name != "" {
		event.AttackerAllianceName = alliance.Name
	}
}

func (s *Service) enrichActor(ctx context.Context, event *notifications.Event) {
	if event == nil || s.resolver == nil || event.ActorCharacterID == "" || event.ActorCharacterName != "" {
		return
	}
	character, err := s.resolver.ResolveCharacter(ctx, event.ActorCharacterID)
	if err != nil {
		s.logger.Warn("alert actor character enrichment failed", "character_id", event.ActorCharacterID, "err", err)
		return
	}
	if character.Name != "" {
		event.ActorCharacterName = character.Name
	}
}

func (s *Service) enrichOwnership(ctx context.Context, event *notifications.Event) {
	if event == nil || event.NotificationType != "OwnershipTransferred" {
		return
	}
	if event.NewOwnerCorporationName == "" && event.NewOwnerCorporationID != "" {
		event.NewOwnerCorporationName = s.resolveCorporationName(ctx, event.NewOwnerCorporationID)
	}
	if event.OldOwnerCorporationName == "" && event.OldOwnerCorporationID != "" {
		event.OldOwnerCorporationName = s.resolveCorporationName(ctx, event.OldOwnerCorporationID)
	}
}

func (s *Service) resolveCorporationName(ctx context.Context, corporationID string) string {
	if s.resolver != nil {
		corporation, err := s.resolver.ResolveCorporation(ctx, corporationID)
		if err != nil {
			s.logger.Warn("alert ownership corporation enrichment failed", "corporation_id", corporationID, "err", err)
			return ""
		}
		return corporation.Name
	}
	name, err := s.resolveName(ctx, "corporation", corporationID)
	if err != nil {
		s.logger.Warn("alert ownership corporation enrichment failed", "corporation_id", corporationID, "err", err)
		return ""
	}
	return name
}

func (s *Service) enrichStructures(ctx context.Context, structures []authnextdb.Structure) {
	if s.resolver == nil {
		return
	}
	for index := range structures {
		structure := &structures[index]
		s.enrichStructureLocation(ctx, structure)
		s.enrichStructureCelestials(ctx, structure)
	}
}

func (s *Service) enrichStructureLocation(ctx context.Context, structure *authnextdb.Structure) {
	if structure == nil || structure.SystemID == "" {
		return
	}
	if structure.SystemName != nil && structure.RegionName != nil {
		return
	}
	location, err := s.resolver.ResolveSolarSystem(ctx, structure.SystemID)
	if err != nil {
		s.logger.Warn("alert structure location enrichment failed", "system_id", structure.SystemID, "err", err)
		return
	}
	if structure.SystemName == nil && location.Name != "" {
		structure.SystemName = new(location.Name)
	}
	if structure.RegionName == nil && location.RegionName != "" {
		structure.RegionName = new(location.RegionName)
	}
}

func (s *Service) enrichStructureCelestials(ctx context.Context, structure *authnextdb.Structure) {
	if structure == nil {
		return
	}
	s.enrichStructurePlanet(ctx, structure)
	s.enrichStructureMoon(ctx, structure)
}

func (s *Service) enrichStructurePlanet(ctx context.Context, structure *authnextdb.Structure) {
	if structure == nil || s.resolver == nil || structure.PlanetID == nil || structure.PlanetName != nil {
		return
	}
	planet, err := s.resolver.ResolvePlanet(ctx, *structure.PlanetID)
	if err != nil {
		s.logger.Warn("alert planet enrichment failed", "planet_id", *structure.PlanetID, "err", err)
		return
	}
	if planet.Name != "" {
		structure.PlanetName = new(planet.Name)
	}
}

func (s *Service) enrichStructureMoon(ctx context.Context, structure *authnextdb.Structure) {
	if structure == nil || s.resolver == nil || structure.MoonID == nil || structure.MoonName != nil {
		return
	}
	moon, err := s.resolver.ResolveMoon(ctx, *structure.MoonID)
	if err != nil {
		s.logger.Warn("alert moon enrichment failed", "moon_id", *structure.MoonID, "err", err)
		return
	}
	if moon.Name != "" {
		structure.MoonName = new(moon.Name)
	}
}

func (s *Service) resolveSystemName(ctx context.Context, systemID string) (string, error) {
	return s.resolveName(ctx, "solar_system", systemID)
}

func (s *Service) resolveName(ctx context.Context, kind, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	resolver, ok := s.database.(NameResolver)
	if !ok {
		return "", nil
	}
	names, err := resolver.ResolveNames(ctx, kind, []string{id})
	if err != nil {
		return "", fmt.Errorf("resolve %s %s: %w", kind, id, err)
	}
	return names[id], nil
}

func destinationSelected(requested map[string]struct{}, destination *Destination) bool {
	if destination == nil {
		return false
	}
	if len(requested) == 0 {
		return true
	}
	if _, ok := requested[destination.ID]; ok {
		return true
	}
	for _, target := range destination.webhookTargets() {
		if _, ok := requested[target.ID]; ok {
			return true
		}
	}
	return false
}

type webhookTarget struct {
	ID  string
	URL string
}

func (d *Destination) webhookTargets() []webhookTarget {
	if d == nil {
		return nil
	}
	urls := d.WebhookURLs
	targets := make([]webhookTarget, 0, len(urls))
	for i, webhookURL := range urls {
		targetID := d.ID
		if len(urls) > 1 {
			targetID = fmt.Sprintf("%s#%d", d.ID, i+1)
		}
		targets = append(targets, webhookTarget{ID: targetID, URL: webhookURL})
	}
	return targets
}

func flattenWebhookTargets(destinations []Destination, requested map[string]struct{}) []webhookTarget {
	flattened := make([]webhookTarget, 0, len(destinations))
	seenURLs := make(map[string]struct{})
	for i := range destinations {
		destination := &destinations[i]
		for _, target := range destination.webhookTargets() {
			if !webhookTargetSelected(requested, destination.ID, target.ID) {
				continue
			}
			key := webhookURLKey(target.URL)
			if _, ok := seenURLs[key]; ok {
				continue
			}
			seenURLs[key] = struct{}{}
			flattened = append(flattened, target)
		}
	}
	return flattened
}

func webhookURLKey(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func webhookTargetSelected(requested map[string]struct{}, destinationID, targetID string) bool {
	if len(requested) == 0 {
		return true
	}
	if _, ok := requested[destinationID]; ok {
		return true
	}
	_, ok := requested[targetID]
	return ok
}

func (s *Service) deliverTarget(ctx context.Context, target *webhookTarget, view *eventView) error {
	if target == nil || view == nil {
		return errors.New("alert webhook target and event view are required")
	}
	message := render(view, s.overrideSenderName, s.overrideSenderAvatarURL)
	startedAt := time.Now()
	s.logger.Debug("Discord alert delivery started",
		"destination_id", target.ID,
		"notification_id", view.Event.NotificationID,
		"alert_type", view.Event.AlertType,
	)
	if err := s.delivery.Deliver(ctx, discord.Destination{ID: target.ID, WebhookURL: target.URL}, &message); err != nil {
		s.logger.Warn("Discord alert delivery failed",
			"destination_id", target.ID,
			"notification_id", view.Event.NotificationID,
			"duration", time.Since(startedAt),
			"err", err,
		)
		return fmt.Errorf("deliver destination %s: %w", target.ID, err)
	}
	if err := s.recordHistory(ctx, target.ID, view, &message); err != nil {
		s.logger.Warn("Discord alert history record failed",
			"destination_id", target.ID,
			"notification_id", view.Event.NotificationID,
			"err", err,
		)
	}
	s.logger.Debug("Discord alert delivery completed",
		"destination_id", target.ID,
		"notification_id", view.Event.NotificationID,
		"duration", time.Since(startedAt),
	)
	return nil
}

func (s *Service) recordHistory(ctx context.Context, destinationID string, view *eventView, message *discord.Message) error {
	if s.history == nil || view == nil || message == nil {
		return nil
	}
	rawNotification := append([]byte(nil), view.RawNotificationJSON...)
	if len(rawNotification) == 0 {
		rawNotification = []byte("null")
	}
	eventJSON, err := json.Marshal(view.Event)
	if err != nil {
		return fmt.Errorf("encode classified event: %w", err)
	}
	discordPayload, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Discord payload: %w", err)
	}
	return s.history.RecordAlert(ctx, &notificationstate.AlertHistoryRecord{
		NotificationID:      view.Event.NotificationID,
		NotificationType:    view.Event.NotificationType,
		AlertType:           view.Event.AlertType,
		CorporationID:       view.PollingCorporationID,
		CorporationName:     view.PollingCorporationName,
		CorporationTicker:   view.PollingCorporationTicker,
		CharacterID:         view.CharacterID,
		DestinationID:       destinationID,
		DispatchedAt:        time.Now().UTC(),
		RawNotificationJSON: rawNotification,
		ClassifiedEventJSON: eventJSON,
		DiscordPayloadJSON:  discordPayload,
	})
}

func compileAlertRouting(includedSelectors, excludedSelectors []string) (alertRouting, error) {
	routing := alertRouting{
		included: make(map[string]struct{}),
		excluded: make(map[string]struct{}),
	}
	if err := addIncludedAlertSelectors(&routing, includedSelectors); err != nil {
		return alertRouting{}, err
	}
	if err := addExcludedAlertSelectors(&routing, excludedSelectors); err != nil {
		return alertRouting{}, err
	}
	return routing, nil
}

func addIncludedAlertSelectors(routing *alertRouting, selectors []string) error {
	for _, selector := range selectors {
		alertTypes, err := expandIncludedAlertSelector(selector)
		if err != nil {
			return fmt.Errorf("include selector %q: %w", selector, err)
		}
		for _, alertType := range alertTypes {
			routing.included[alertType] = struct{}{}
		}
	}
	return nil
}

func expandIncludedAlertSelector(selector string) ([]string, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if selector == "all" || selector == "*" {
		return notifications.AllAlertTypes(), nil
	}
	return notifications.ExpandAlertSelector(selector)
}

func addExcludedAlertSelectors(routing *alertRouting, selectors []string) error {
	for _, selector := range selectors {
		alertType, err := resolveExcludedAlertSelector(selector)
		if err != nil {
			return err
		}
		routing.excluded[alertType] = struct{}{}
	}
	return nil
}

func resolveExcludedAlertSelector(selector string) (string, error) {
	canonical, err := notifications.NormalizeAlertSelector(selector)
	if err != nil {
		return "", fmt.Errorf("exclude selector %q: %w", selector, err)
	}
	alertTypes, err := notifications.ExpandAlertSelector(canonical)
	if err != nil {
		return "", fmt.Errorf("exclude selector %q: %w", selector, err)
	}
	if notifications.IsAlertGroup(canonical) || len(alertTypes) != 1 {
		return "", fmt.Errorf("exclude selector %q is not a leaf", selector)
	}
	return alertTypes[0], nil
}

func (r alertRouting) supports(alertType string) bool {
	if _, excluded := r.excluded[alertType]; excluded {
		return false
	}
	_, included := r.included[alertType]
	return included
}

func compileCorporationFilter(included, excluded []string) corporationFilter {
	filter := corporationFilter{
		included: make(map[string]struct{}, len(included)),
		excluded: make(map[string]struct{}, len(excluded)),
	}
	for _, corporationID := range included {
		filter.included[strings.TrimSpace(corporationID)] = struct{}{}
	}
	for _, corporationID := range excluded {
		filter.excluded[strings.TrimSpace(corporationID)] = struct{}{}
	}
	return filter
}

func (f corporationFilter) supports(corporationID string) bool {
	corporationID = strings.TrimSpace(corporationID)
	if _, excluded := f.excluded[corporationID]; excluded {
		return false
	}
	if len(f.included) == 0 {
		return true
	}
	_, included := f.included[corporationID]
	return included
}

func supports(alertTypes, excludedAlertTypes []string, alertType string) bool {
	routing, err := compileAlertRouting(alertTypes, excludedAlertTypes)
	return err == nil && routing.supports(alertType)
}

func destinationExcludesStructureType(destination *Destination, view *eventView) bool {
	if destination == nil || view == nil || len(destination.ExcludeStructureTypeIDs) == 0 {
		return false
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		for i := range view.Structures {
			if slices.Contains(destination.ExcludeStructureTypeIDs, strings.TrimSpace(view.Structures[i].TypeID)) {
				return true
			}
		}
		return false
	}
	if typeID := strings.TrimSpace(view.Event.StructureTypeID); typeID != "" {
		return slices.Contains(destination.ExcludeStructureTypeIDs, typeID)
	}
	for i := range view.Structures {
		if slices.Contains(destination.ExcludeStructureTypeIDs, strings.TrimSpace(view.Structures[i].TypeID)) {
			return true
		}
	}
	return false
}

func structureTypeID(view *eventView) string {
	if view == nil {
		return ""
	}
	if typeID := strings.TrimSpace(view.Event.StructureTypeID); typeID != "" {
		return typeID
	}
	for i := range view.Structures {
		if typeID := strings.TrimSpace(view.Structures[i].TypeID); typeID != "" {
			return typeID
		}
	}
	return ""
}

func structureTypeIDFromStructures(typeID string, structures []authnextdb.Structure) string {
	if typeID = strings.TrimSpace(typeID); typeID != "" {
		return typeID
	}
	for index := range structures {
		if typeID := strings.TrimSpace(structures[index].TypeID); typeID != "" {
			return typeID
		}
	}
	return ""
}

func structureTypeIDForEvent(event *notifications.Event, structures []authnextdb.Structure) string {
	if event == nil {
		return ""
	}
	if event.NotificationType == "StructuresReinforcementChanged" {
		return ""
	}
	return structureTypeIDFromStructures(event.StructureTypeID, structures)
}

func (s *Service) logDestinationsResolved(request *DeliveryRequest, count int) {
	attrs := []any{
		"notification_id", request.Event.NotificationID,
		"alert_type", request.Event.AlertType,
		"destination_count", count,
	}
	if s.logPayloads && len(request.RawNotificationJSON) > 0 {
		attrs = append(attrs, "raw_payload", logPayload(request.RawNotificationJSON))
	}
	s.logger.Debug("alert destinations resolved", attrs...)
}

func logPayload(payload []byte) string {
	if len(payload) <= maxLoggedPayloadBytes {
		return string(payload)
	}
	return string(payload[:maxLoggedPayloadBytes]) + "...[truncated]"
}

type eventView struct {
	CorporationID            string
	CorporationName          string
	CorporationTicker        string
	CorporationOwned         bool
	SystemName               string
	RegionID                 string
	RegionName               string
	AllianceName             string
	AllianceIconURL          string
	ShowEntityIDs            bool
	PlanetName               string
	MoonName                 string
	Event                    notifications.Event
	Structures               []authnextdb.Structure
	StructureTypeName        string
	StructureTypeNames       map[string]string
	CharacterID              string
	RawNotificationJSON      []byte
	PollingCorporationID     string
	PollingCorporationName   string
	PollingCorporationTicker string
}

type reinforcementCoverageRegion struct {
	label   string
	systems map[string]reinforcementCoverageSystem
}

type reinforcementCoverageSystem struct {
	label string
	types map[string]reinforcementCoverageType
}

type reinforcementCoverageType struct {
	label string
	count int
}

func render(view *eventView, senderName, avatarURL string) discord.Message {
	if view == nil {
		return discord.Message{}
	}
	title := escapeMarkdown(notifications.DisplayName(view.Event.NotificationType))
	description := eventDescription(view)
	description = truncate(description, maxTextSize)
	fields := buildFields(view)
	color := alertColor(&view.Event)
	author := eventAuthor(view)
	return discord.Message{
		Username:  senderName,
		AvatarURL: avatarURL,
		Embeds: []discord.Embed{{
			Title:       title,
			Description: description,
			Color:       color,
			Timestamp:   view.Event.Timestamp,
			Fields:      fields,
			Thumbnail:   structureThumbnail(view.Structures, view.Event.StructureTypeID),
			Author:      author,
		}},
		AllowedMentions: discord.AllowedMentions{Parse: []string{}},
	}
}

func buildFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}

	fields := make([]discord.Field, 0, discord.MaxEmbedFields)
	for _, renderFields := range eventFieldRenderers {
		fields = append(fields, renderFields(view)...)
	}
	if len(fields) > discord.MaxEmbedFields {
		return fields[:discord.MaxEmbedFields]
	}
	return fields
}

func corporationFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if !view.CorporationOwned {
		return nil
	}
	return []discord.Field{{Name: "Corporation", Value: corporationLabel(view.CorporationID, view.CorporationName, view.CorporationTicker, view.ShowEntityIDs), Inline: true}}
}

func ownershipFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType != "OwnershipTransferred" {
		return nil
	}
	previousOwner := corporationLabel(view.Event.OldOwnerCorporationID, view.Event.OldOwnerCorporationName, "", view.ShowEntityIDs)
	newOwner := corporationLabel(view.Event.NewOwnerCorporationID, view.Event.NewOwnerCorporationName, "", view.ShowEntityIDs)
	if previousOwner == "" && newOwner == "" {
		return nil
	}
	lines := make([]string, 0, maxOwnershipParts)
	if previousOwner != "" {
		lines = append(lines, "Previous owner: "+previousOwner)
	}
	if newOwner != "" {
		lines = append(lines, "New owner: "+newOwner)
	}
	return []discord.Field{{Name: "Ownership", Value: strings.Join(lines, "\n"), Inline: false}}
}

func whenFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	return []discord.Field{{Name: "When", Value: timestampValue(view.Event.Timestamp), Inline: false}}
}

func attackerFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	lines := make([]string, 0, maxAttackerParts)
	if character := attackerIdentityLabel(view.Event.AttackerCharacterName, view.Event.AttackerCharacterID, "Character", view.ShowEntityIDs); character != "" {
		lines = append(lines, character)
	}
	if corporation := attackerIdentityLabel(view.Event.AttackerCorporationName, view.Event.AttackerCorporationID, "Corporation", view.ShowEntityIDs); corporation != "" {
		lines = append(lines, corporation)
	}
	if alliance := eveWhoLabel(eveWhoAllianceURL, "Alliance", view.Event.AttackerAllianceID, view.Event.AttackerAllianceName, view.ShowEntityIDs); alliance != "" {
		lines = append(lines, "Alliance: "+alliance)
	}
	if len(lines) == 0 {
		return nil
	}
	return []discord.Field{{Name: "Attacker", Value: strings.Join(lines, "\n"), Inline: true}}
}

func actorFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	actor := attackerIdentityLabel(view.Event.ActorCharacterName, view.Event.ActorCharacterID, "Character", view.ShowEntityIDs)
	if actor == "" {
		return nil
	}
	return []discord.Field{{Name: "Actor", Value: actor, Inline: true}}
}

func allianceFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	alliance := view.AllianceName
	if alliance == "" {
		alliance = view.Event.AttackerAllianceName
	}
	if view.Event.AllianceID == "" {
		alliance = escapeMarkdown(strings.TrimSpace(alliance))
	} else {
		alliance = eveWhoLabel(eveWhoAllianceURL, "Alliance", view.Event.AllianceID, alliance, view.ShowEntityIDs)
	}
	if alliance != "" {
		return []discord.Field{{Name: "Alliance", Value: alliance, Inline: true}}
	}
	return nil
}

func systemFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" && view.Event.SystemID == "" {
		return nil
	}
	system := view.SystemName
	if view.Event.SystemID == "" {
		system = escapeMarkdown(strings.TrimSpace(system))
	} else {
		system = dotlanLabel(dotlanSystemURL, "System", view.Event.SystemID, system, view.ShowEntityIDs)
	}
	if system != "" {
		return []discord.Field{{Name: "Solar System", Value: system, Inline: true}}
	}
	return nil
}

func regionFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" && view.Event.SystemID == "" {
		return nil
	}
	region := view.RegionName
	if view.RegionID == "" {
		region = escapeMarkdown(strings.TrimSpace(region))
	} else {
		region = dotlanLabel(dotlanRegionURL, "Region", view.RegionID, region, view.ShowEntityIDs)
	}
	if region != "" {
		return []discord.Field{{Name: "Region", Value: region, Inline: true}}
	}
	return nil
}

func reinforcementSystemLabels(view *eventView) []string {
	if view == nil {
		return nil
	}
	labels := make(map[string]string, len(view.Structures))
	for index := range view.Structures {
		structure := &view.Structures[index]
		id := strings.TrimSpace(structure.SystemID)
		name := optionalString(structure.SystemName)
		label := systemLabel(id, name, view.ShowEntityIDs)
		if label == "" {
			continue
		}
		key := id
		if key == "" {
			key = name
		}
		labels[key] = label
	}
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		out = append(out, label)
	}
	slices.Sort(out)
	return out
}

func reinforcementCoverageField(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	regions := reinforcementCoverageGroups(view)
	if len(regions) == 0 {
		return nil
	}
	regionKeys := make([]string, 0, len(regions))
	for regionKey := range regions {
		regionKeys = append(regionKeys, regionKey)
	}
	slices.Sort(regionKeys)
	lines := make([]string, 0)
	for _, regionKey := range regionKeys {
		region := regions[regionKey]
		lines = append(lines, region.label)
		lines = append(lines, reinforcementCoverageSystemLines(region)...)
	}
	return []discord.Field{{Name: "Structure Coverage", Value: truncate(strings.Join(lines, "\n"), maxStructureSummarySize), Inline: false}}
}

func reinforcementCoverageGroups(view *eventView) map[string]*reinforcementCoverageRegion {
	regions := make(map[string]*reinforcementCoverageRegion, len(view.Structures))
	for index := range view.Structures {
		structure := &view.Structures[index]
		regionKey, regionLabel, systemKey, systemLabel := reinforcementCoverageLocation(view, structure)
		region := regions[regionKey]
		if region == nil {
			region = &reinforcementCoverageRegion{label: regionLabel, systems: make(map[string]reinforcementCoverageSystem)}
			regions[regionKey] = region
		}
		system := region.systems[systemKey]
		if system.label == "" {
			system.label = systemLabel
			system.types = make(map[string]reinforcementCoverageType)
		}
		typeKey, typeLabel := reinforcementCoverageTypeLabel(view, structure)
		structureType := system.types[typeKey]
		structureType.label = typeLabel
		structureType.count++
		system.types[typeKey] = structureType
		region.systems[systemKey] = system
	}
	return regions
}

func reinforcementCoverageLocation(view *eventView, structure *authnextdb.Structure) (regionKey, regionDisplay, systemKey, systemDisplay string) {
	systemID := strings.TrimSpace(structure.SystemID)
	systemName := optionalString(structure.SystemName)
	systemDisplay = systemLabel(systemID, systemName, view.ShowEntityIDs)
	if systemDisplay == "" {
		systemDisplay = "Unknown system"
	}
	regionID := strings.TrimSpace(optionalString(structure.RegionID))
	regionName := optionalString(structure.RegionName)
	regionDisplay = regionLabel(regionID, regionName, view.ShowEntityIDs)
	if regionDisplay == "" {
		regionDisplay = "Unknown region"
	}
	regionKey = regionID
	if regionKey == "" {
		regionKey = regionDisplay
	}
	systemKey = systemID
	if systemKey == "" {
		systemKey = systemDisplay
	}
	return regionKey, regionDisplay, systemKey, systemDisplay
}

func reinforcementCoverageTypeLabel(view *eventView, structure *authnextdb.Structure) (key, label string) {
	typeID := strings.TrimSpace(structure.TypeID)
	label = ""
	if view.StructureTypeNames != nil {
		label = strings.TrimSpace(view.StructureTypeNames[typeID])
	}
	if label == "" {
		label = strings.TrimSpace(optionalString(structure.TypeName))
	}
	if label == "" && typeID != "" {
		label = "Structure type " + typeID
	}
	if label == "" {
		label = "Unknown structure type"
	}
	key = typeID
	if key == "" {
		key = label
	}
	return key, label
}

func reinforcementCoverageSystemLines(region *reinforcementCoverageRegion) []string {
	if region == nil {
		return nil
	}
	systemKeys := make([]string, 0, len(region.systems))
	for systemKey := range region.systems {
		systemKeys = append(systemKeys, systemKey)
	}
	slices.Sort(systemKeys)
	lines := make([]string, 0, len(systemKeys))
	for _, systemKey := range systemKeys {
		system := region.systems[systemKey]
		lines = append(lines, "  "+system.label)
		typeKeys := reinforcementCoverageTypeKeys(system)
		for _, typeKey := range typeKeys {
			coverage := system.types[typeKey]
			noun := "structures"
			if coverage.count == 1 {
				noun = "structure"
			}
			lines = append(lines, fmt.Sprintf("    %s - %d %s", coverage.label, coverage.count, noun))
		}
	}
	return lines
}

func reinforcementCoverageTypeKeys(system reinforcementCoverageSystem) []string {
	keys := make([]string, 0, len(system.types))
	for typeKey := range system.types {
		keys = append(keys, typeKey)
	}
	slices.SortFunc(keys, func(left, right string) int {
		if comparison := strings.Compare(system.types[left].label, system.types[right].label); comparison != 0 {
			return comparison
		}
		return strings.Compare(left, right)
	})
	return keys
}

func planetFields(view *eventView) []discord.Field {
	if view == nil || !notifications.AlertTypeInGroup(view.Event.AlertType, notifications.AlertCustomsOffices) {
		return nil
	}
	planet := strings.TrimSpace(view.PlanetName)
	if planet == "" {
		if view.Event.PlanetID == "" {
			return nil
		}
		planet = "Planet " + view.Event.PlanetID
	} else {
		planet = escapeMarkdown(planet)
		if view.ShowEntityIDs && view.Event.PlanetID != "" {
			planet += " (" + escapeMarkdown(view.Event.PlanetID) + ")"
		}
	}
	return []discord.Field{{Name: "Planet", Value: planet, Inline: true}}
}

func structureFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		return reinforcementCoverageField(view)
	}
	lines := make([]string, 0, len(view.Structures))
	for index := range view.Structures {
		lines = append(lines, formatStructure(&view.Structures[index], view))
	}
	if len(lines) == 0 {
		for _, structureID := range view.Event.StructureIDs {
			lines = append(lines, fallbackStructureLine(structureID, view))
		}
	}
	if structures := truncate(strings.Join(lines, "\n"), maxStructureSummarySize); structures != "" {
		return []discord.Field{{Name: structureFieldName(view.Event.NotificationType), Value: structures, Inline: false}}
	}
	return nil
}

func structureTypeFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.NotificationType == "StructuresReinforcementChanged" {
		return nil
	}
	structureType := strings.TrimSpace(view.StructureTypeName)
	if structureType == "" {
		if view.Event.StructureTypeID == "" {
			return nil
		}
		structureType = "Structure type " + view.Event.StructureTypeID
	} else {
		structureType = escapeMarkdown(structureType)
		if view.ShowEntityIDs && view.Event.StructureTypeID != "" {
			structureType += " (" + escapeMarkdown(view.Event.StructureTypeID) + ")"
		}
	}
	if structureType != "" {
		return []discord.Field{{Name: "Structure Type", Value: structureType, Inline: true}}
	}
	return nil
}

func integrityFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	values := make([]string, 0, maxIntegrityValues)
	if view.Event.ShieldPercentage != nil {
		values = append(values, fmt.Sprintf("Shields: **%.1f%%**", *view.Event.ShieldPercentage))
	}
	if view.Event.ArmorPercentage != nil {
		values = append(values, fmt.Sprintf("Armor: **%.1f%%**", *view.Event.ArmorPercentage))
	}
	if view.Event.HullPercentage != nil {
		values = append(values, fmt.Sprintf("Hull: **%.1f%%**", *view.Event.HullPercentage))
	}
	if view.Event.ShieldValue != nil {
		values = append(values, "Shield value: **"+formatIntegrityValue(*view.Event.ShieldValue)+"**")
	}
	if view.Event.ArmorValue != nil {
		values = append(values, "Armor value: **"+formatIntegrityValue(*view.Event.ArmorValue)+"**")
	}
	if view.Event.HullValue != nil {
		values = append(values, "Hull value: **"+formatIntegrityValue(*view.Event.HullValue)+"**")
	}
	if len(values) > 0 {
		return []discord.Field{{Name: "Integrity", Value: strings.Join(values, "\n"), Inline: true}}
	}
	return nil
}

func resourceFields(view *eventView) []discord.Field {
	if view == nil || len(view.Event.ResourceRequirements) == 0 {
		return nil
	}
	lines := make([]string, 0, len(view.Event.ResourceRequirements))
	for _, requirement := range view.Event.ResourceRequirements {
		if requirement.TypeID == "" || requirement.Quantity <= 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s x Type %s", formatInteger(requirement.Quantity), escapeMarkdown(requirement.TypeID)))
	}
	if len(lines) == 0 {
		return nil
	}
	return []discord.Field{{Name: "Resources Needed", Value: strings.Join(lines, "\n"), Inline: false}}
}

func decloakFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	if view.Event.DecloakAt.IsZero() {
		return nil
	}
	return []discord.Field{{Name: "Decloak At", Value: timestampValue(view.Event.DecloakAt), Inline: true}}
}

func structureFieldName(notificationType string) string {
	if notificationType == "OwnershipTransferred" {
		return "Structure"
	}
	return "Structures"
}

func eventAuthor(view *eventView) *discord.Author {
	if view == nil {
		return nil
	}
	if view.CorporationOwned {
		author := &discord.Author{Name: escapeMarkdown(view.CorporationName)}
		if view.CorporationID != "" {
			author.IconURL = fmt.Sprintf(corporationLogoURL, view.CorporationID)
		}
		return author
	}
	if view.Event.AllianceID != "" {
		name := view.AllianceName
		if name == "" {
			name = view.Event.AllianceName
		}
		if name == "" {
			return nil
		}
		iconURL := view.AllianceIconURL
		if iconURL == "" {
			iconURL = fmt.Sprintf(allianceLogoURL, view.Event.AllianceID)
		}
		return &discord.Author{
			Name:    escapeMarkdown(name),
			IconURL: iconURL,
		}
	}
	return nil
}

func alertColor(event *notifications.Event) int {
	if event == nil {
		return colorDanger
	}
	switch {
	case isGainingSovereignty(event.NotificationType), event.AlertType == notifications.AlertStructureOnline, event.NotificationType == "StructureOnline", event.AlertType == notifications.AlertStructureWentHighPower, event.NotificationType == "StructureWentHighPower":
		return colorSuccess
	case event.NotificationType == "StructureAnchoring", event.AlertType == notifications.AlertStructureAnchoring, event.NotificationType == "SkyhookOnline", event.NotificationType == "SkyhookDeployed", event.NotificationType == "StationServiceEnabled":
		return colorInformational
	case event.NotificationType == "OwnershipTransferred":
		return colorInformational
	case event.NotificationType == "StructureUnderAttack", event.NotificationType == "SkyhookUnderAttack", event.NotificationType == "MercenaryDenAttacked", event.NotificationType == "StructureLostShields", event.NotificationType == "StructureLostArmor", event.NotificationType == "SovStructureSelfDestructRequested", event.NotificationType == "OrbitalAttacked", event.NotificationType == "OrbitalReinforced":
		return colorDanger
	case event.AlertType == notifications.AlertESSMainBankLink, event.NotificationType == "ESSMainBankLink", event.AlertType == notifications.AlertStructureNoReagents, event.NotificationType == "StructureNoReagentsAlert":
		return colorDanger
	case event.NotificationType == "TowerAlertMsg":
		return colorDanger
	case notifications.AlertTypeInGroup(event.AlertType, notifications.AlertESS):
		return colorWarning
	case event.AlertType == notifications.AlertStructureUnanchoring, event.NotificationType == "StructureUnanchoring", event.AlertType == notifications.AlertSovStationExitedReinforce, event.NotificationType == "SovStationExitedReinforce", event.AlertType == notifications.AlertStructuresReinforcementChanged, event.NotificationType == "StructuresReinforcementChanged", event.AlertType == notifications.AlertStructureVulnerable, event.NotificationType == "StructureVulnerable", event.AlertType == notifications.AlertSovStructureSelfDestructCancel, event.NotificationType == "SovStructureSelfDestructCancel", notifications.AlertTypeInGroup(event.AlertType, notifications.AlertStructureFuel), notifications.AlertTypeInGroup(event.AlertType, notifications.AlertStarbase), event.NotificationType == "StructureServicesOffline", event.NotificationType == "StructureWentLowPower", event.NotificationType == "StationServiceDisabled", event.NotificationType == "MercenaryDenNewMTO":
		return colorWarning
	default:
		return colorDanger
	}
}

func isGainingSovereignty(notificationType string) bool {
	return notificationType == "SovAllClaimAquiredMsg" || notificationType == "SovereigntyClaimed"
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func formatInteger(value int64) string {
	return strconv.FormatInt(value, 10)
}

func formatIntegrityValue(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func corporationLabel(id, name, ticker string, showEntityIDs bool) string {
	label := eveWhoLabel(eveWhoCorporationURL, "Corporation", id, name, showEntityIDs)
	if ticker == "" {
		return label
	}
	return label + " [" + escapeMarkdown(ticker) + "]"
}

func timestampValue(timestamp time.Time) string {
	return discordTimestamp(timestamp, discordTimestampFull) + "\nEVE Time: " + eveTime(timestamp)
}

func timeRemainingValue(start time.Time, remaining time.Duration) string {
	if remaining <= 0 || start.IsZero() {
		return ""
	}
	expires := start.Add(remaining)
	return discordTimestamp(expires, discordTimestampRelative) + "\nEVE Time: " + eveTime(expires)
}

func discordTimestamp(timestamp time.Time, style string) string {
	return fmt.Sprintf("<t:%d:%s>", timestamp.Unix(), style)
}

func eveTime(timestamp time.Time) string {
	return timestamp.UTC().Format("15:04:05 UTC")
}

func systemLabel(systemID, systemName string, showEntityIDs bool) string {
	if systemID == "" {
		return escapeMarkdown(strings.TrimSpace(systemName))
	}
	return dotlanLabel(dotlanSystemURL, "System", systemID, systemName, showEntityIDs)
}

func regionLabel(regionID, regionName string, showEntityIDs bool) string {
	if regionID == "" {
		return escapeMarkdown(strings.TrimSpace(regionName))
	}
	return dotlanLabel(dotlanRegionURL, "Region", regionID, regionName, showEntityIDs)
}

func fallbackStructureLine(structureID string, view *eventView) string {
	if view == nil {
		return ""
	}
	systemName := view.SystemName
	if systemName == "" {
		systemName = "unknown system"
	}
	structureName := view.Event.StructureName
	if structureName == "" {
		structureName = "Structure " + structureID
	}
	line := escapeMarkdown(structureName) + " in " + escapeMarkdown(systemName)
	if view.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(view.PlanetName)
	}
	if view.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(view.MoonName)
	}
	return line
}

func eventDescription(view *eventView) string {
	if view == nil {
		return ""
	}
	event := &view.Event
	system := systemLabel(event.SystemID, view.SystemName, view.ShowEntityIDs)
	if system == "" {
		system = "the reported system"
	}
	if event.NotificationType == "SkyhookUnderAttack" {
		return "A skyhook is under attack in " + system + "."
	}
	if event.NotificationType == "StructuresReinforcementChanged" && event.ReinforcedStructureCount != nil {
		systems := reinforcementSystemLabels(view)
		if len(systems) > 1 {
			return fmt.Sprintf("The reinforcement schedule changed for %d structures across %d solar systems.", *event.ReinforcedStructureCount, len(systems))
		}
		if len(systems) == 1 {
			return fmt.Sprintf("The reinforcement schedule changed for %d structures in %s.", *event.ReinforcedStructureCount, systems[0])
		}
		return fmt.Sprintf("The reinforcement schedule changed for %d structures across the reported solar systems.", *event.ReinforcedStructureCount)
	}
	if template, ok := eventDescriptionTemplates[event.NotificationType]; ok {
		return fmt.Sprintf(template, system)
	}
	if summary := strings.TrimSpace(event.Summary); summary != "" {
		return escapeMarkdown(summary)
	}
	return escapeMarkdown(notifications.DisplayName(event.NotificationType)) + "."
}

func isSkyhookReinforced(event *notifications.Event) bool {
	return event != nil && event.NotificationType == "SkyhookLostShields"
}

func timerFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	event := &view.Event
	fields := make([]discord.Field, 0, maxTimerFields)
	if !event.ReinforcedUntil.IsZero() {
		fields = append(fields, discord.Field{Name: "Reinforced Until", Value: timestampValue(event.ReinforcedUntil), Inline: true})
	}
	if remaining := timeRemainingValue(event.Timestamp, event.TimeLeft); remaining != "" {
		fieldName := "Time Remaining"
		if isSkyhookReinforced(event) && len(fields) == 0 {
			fieldName = "Reinforced Until"
		}
		fields = append(fields, discord.Field{Name: fieldName, Value: remaining, Inline: true})
	}
	if vulnerable := timeRemainingValue(event.Timestamp, event.VulnerableFor); vulnerable != "" && !isSkyhookReinforced(event) {
		fields = append(fields, discord.Field{Name: "Vulnerable Until", Value: vulnerable, Inline: true})
	}
	return fields
}

func activityFields(view *eventView) []discord.Field {
	if view == nil {
		return nil
	}
	event := &view.Event
	fields := make([]discord.Field, 0, maxActivityFields)
	if !event.ReadyAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Ready At", Value: timestampValue(event.ReadyAt), Inline: true})
	}
	if !event.AutoAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Automatic Fracture At", Value: timestampValue(event.AutoAt), Inline: true})
	}
	if !event.DestructAt.IsZero() {
		fields = append(fields, discord.Field{Name: "Self-Destruct At", Value: timestampValue(event.DestructAt), Inline: true})
	}
	if event.AbandonAfterDays > 0 {
		fields = append(fields, discord.Field{Name: "Assets at Risk", Value: fmt.Sprintf("in %d days", event.AbandonAfterDays), Inline: true})
	}
	if event.ReinforcementHour != nil || event.ReinforcementWeekday != nil || event.ReinforcedStructureCount != nil {
		lines := make([]string, 0, maxScheduleValues)
		if event.ReinforcedStructureCount != nil {
			lines = append(lines, fmt.Sprintf("Structures: %d", *event.ReinforcedStructureCount))
		}
		if event.ReinforcementWeekday != nil {
			lines = append(lines, "Weekday: "+reinforcementWeekdayLabel(*event.ReinforcementWeekday))
		}
		if event.ReinforcementHour != nil {
			lines = append(lines, fmt.Sprintf("Hour: %02d:00 EVE", *event.ReinforcementHour))
		}
		fields = append(fields, discord.Field{Name: "Reinforcement Schedule", Value: strings.Join(lines, "\n"), Inline: true})
	}
	return fields
}

func reinforcementWeekdayLabel(weekday int) string {
	if weekday == reinforcementWeekdayUnchanged {
		return "Unchanged"
	}
	if weekday < 0 || weekday >= 7 {
		return fmt.Sprintf("Unknown (%d)", weekday)
	}
	return [...]string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}[weekday]
}

func attackerIdentityLabel(name, id, fallback string, showEntityIDs bool) string {
	if fallback == "Character" {
		return eveWhoLabel(eveWhoCharacterURL, fallback, id, name, showEntityIDs)
	}
	return eveWhoLabel(eveWhoCorporationURL, fallback, id, name, showEntityIDs)
}

func formatStructure(structure *authnextdb.Structure, view *eventView) string {
	if structure == nil || view == nil {
		return ""
	}
	name := optionalString(structure.Name)
	if name == "" {
		name = view.Event.StructureName
	}
	if name == "" {
		name = "Structure " + structure.ID
	}
	system := systemLabel(structure.SystemID, optionalString(structure.SystemName), view.ShowEntityIDs)
	if system == "" {
		system = "System " + structure.SystemID
	}
	if region := regionLabel(optionalString(structure.RegionID), optionalString(structure.RegionName), view.ShowEntityIDs); region != "" {
		system += " / " + region
	}
	line := escapeMarkdown(name) + " in " + system
	if structure.PlanetName != nil && *structure.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(*structure.PlanetName)
	} else if view.PlanetName != "" {
		line += "\nPlanet: " + escapeMarkdown(view.PlanetName)
	}
	if structure.MoonName != nil && *structure.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(*structure.MoonName)
	} else if view.MoonName != "" {
		line += "\nMoon: " + escapeMarkdown(view.MoonName)
	}
	return line
}

func structureThumbnail(structures []authnextdb.Structure, fallbackTypeID string) *discord.Image {
	typeID := fallbackTypeID
	if typeID == "" && len(structures) == 1 && structures[0].TypeID != "" {
		typeID = structures[0].TypeID
	}
	if typeID == "" {
		return nil
	}
	return &discord.Image{URL: "https://images.evetech.net/types/" + typeID + "/render?size=64"}
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-3]) + "..."
}

func eveWhoLabel(template, fallback, id, name string, showEntityIDs bool) string {
	return linkedLabel(template, fallback, id, name, false, showEntityIDs)
}

func dotlanLabel(template, fallback, id, name string, showEntityIDs bool) string {
	return linkedLabel(template, fallback, id, name, true, showEntityIDs)
}

func linkedLabel(template, fallback, id, name string, dotlan, showEntityIDs bool) string {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" {
		return escapeMarkdown(name)
	}
	label := name
	if label == "" {
		label = fallback + " " + id
	}
	pathID := id
	if dotlan && name != "" {
		pathID = strings.Join(strings.Fields(name), "_")
	}
	linked := fmt.Sprintf("[%s](%s)", escapeMarkdown(label), fmt.Sprintf(template, url.PathEscape(pathID)))
	if name == "" {
		return linked
	}
	if !showEntityIDs {
		return linked
	}
	return linked + " (" + escapeMarkdown(id) + ")"
}

func escapeMarkdown(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"*", "\\*",
		"_", "\\_",
		"~", "\\~",
		"`", "\\`",
		">", "\\>",
		"|", "\\|",
		"[", "\\[",
		"]", "\\]",
	).Replace(value)
}
