// Package enrichment builds provider-neutral notification contexts.
package enrichment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btnmasher/rex/internal/authnextdb"
	"github.com/btnmasher/rex/internal/notifications"
	"github.com/btnmasher/rex/internal/universe"
)

// Database supplies tracked structures and optional local universe names.
type Database interface {
	GetStructuresByIDs(context.Context, []string) ([]authnextdb.Structure, error)
}

// NameDatabase supplies optional name lookups from the local universe database.
type NameDatabase interface {
	ResolveNames(context.Context, string, []string) (map[string]string, error)
}

// Enricher builds provider-neutral notification contexts for destination adapters.
type Enricher interface {
	Enrich(context.Context, *Envelope) (*Context, error)
}

// Envelope is the immutable notification input shared by all destinations.
type Envelope struct {
	Corporation         authnextdb.Corporation
	CharacterID         string
	RawNotificationJSON []byte
	Event               notifications.Event
}

// Context contains the classified notification and best-effort enriched data.
// Destination adapters may read this value but must not mutate it.
type Context struct {
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

// Service resolves notification data from tracked structures, local universe
// data, and the configured public universe resolver.
type Service struct {
	database Database
	resolver universe.Resolver
	logger   *slog.Logger
}

// NewService creates an enrichment service. Database and resolver are optional;
// missing enrichment remains non-fatal to notification delivery.
func NewService(database Database, resolver universe.Resolver, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{database: database, resolver: resolver, logger: logger}
}

// Enrich creates a provider-neutral context using best-effort lookups.
func (s *Service) Enrich(ctx context.Context, envelope *Envelope) (*Context, error) {
	if s == nil {
		return nil, errors.New("enrichment service is unavailable")
	}
	if ctx == nil {
		return nil, errors.New("enrichment context is required")
	}
	if envelope == nil {
		return nil, errors.New("notification envelope is required")
	}
	event := envelope.Event
	structures := s.loadStructures(ctx, event.StructureIDs, event.NotificationID)
	s.enrichStructures(ctx, structures)
	event.StructureTypeID = structureTypeIDForEvent(&event, structures)
	structureTypeName := s.resolveStructureTypeName(ctx, event.StructureTypeID, structures)
	structureTypeNames := s.resolveEventStructureTypeNames(ctx, &event, structures)
	location := s.resolveLocation(ctx, event.SystemID)

	if event.AllianceID == "" && location.AllianceID != "" {
		event.AllianceID = location.AllianceID
		event.AllianceName = location.AllianceName
	}
	if event.AllianceID == "" && strings.EqualFold(event.SenderType, "alliance") && event.SenderID > 0 {
		event.AllianceID = fmt.Sprintf("%d", event.SenderID)
	}
	allianceName, allianceIconURL := s.resolveAlliance(ctx, &event)
	s.enrichAttacker(ctx, &event)
	s.enrichActor(ctx, &event)
	s.enrichOwnership(ctx, &event)

	view := &Context{
		CorporationID:            envelope.Corporation.ID,
		CorporationName:          envelope.Corporation.Name,
		CorporationTicker:        envelope.Corporation.Ticker,
		CorporationOwned:         len(event.StructureIDs) > 0 || event.OwnerCorporationID != "" || notifications.AlertTypeInGroup(event.AlertType, notifications.AlertStarbase) || notifications.AlertTypeInGroup(event.AlertType, notifications.AlertCustomsOffices),
		SystemName:               location.Name,
		RegionID:                 location.RegionID,
		RegionName:               location.RegionName,
		AllianceName:             allianceName,
		AllianceIconURL:          allianceIconURL,
		ShowEntityIDs:            false,
		PlanetName:               s.resolveCelestialName(ctx, "planet", event.PlanetID),
		MoonName:                 s.resolveCelestialName(ctx, "moon", event.MoonID),
		Event:                    event,
		Structures:               structures,
		CharacterID:              envelope.CharacterID,
		RawNotificationJSON:      append([]byte(nil), envelope.RawNotificationJSON...),
		PollingCorporationID:     envelope.Corporation.ID,
		PollingCorporationName:   envelope.Corporation.Name,
		PollingCorporationTicker: envelope.Corporation.Ticker,
		StructureTypeName:        structureTypeName,
		StructureTypeNames:       structureTypeNames,
	}
	if event.OwnerCorporationID != "" {
		view.CorporationID = event.OwnerCorporationID
		if event.OwnerCorporationID != envelope.Corporation.ID {
			view.CorporationTicker = ""
		}
	}
	if event.OwnerCorporationName != "" {
		view.CorporationName = event.OwnerCorporationName
	}
	s.logger.Debug("notification enrichment completed",
		"notification_id", event.NotificationID,
		"structure_count", len(structures),
	)
	return view, nil
}

func (s *Service) loadStructures(ctx context.Context, structureIDs []string, notificationID int64) []authnextdb.Structure {
	if s == nil || s.database == nil || len(structureIDs) == 0 {
		return nil
	}
	structures, err := s.database.GetStructuresByIDs(ctx, structureIDs)
	if err != nil {
		s.logger.Warn("alert structure enrichment failed", "notification_id", notificationID, "err", err)
		return nil
	}
	return structures
}

func (s *Service) resolveCelestialName(ctx context.Context, kind, id string) string {
	if id == "" {
		return ""
	}
	entity, err := s.resolveCelestial(ctx, kind, id)
	if err != nil {
		s.logger.Warn("alert celestial enrichment failed", "kind", kind, "id", id, "err", err)
		return ""
	}
	return entity.Name
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
		if err == nil {
			return entity.Name
		}
		s.logger.Warn("alert structure type enrichment failed", "type_id", typeID, "err", err)
	}
	return s.resolveName(ctx, "type", typeID)
}

func (s *Service) resolveStructureTypeNames(ctx context.Context, structures []authnextdb.Structure) map[string]string {
	names := make(map[string]string, len(structures))
	for index := range structures {
		typeID := strings.TrimSpace(structures[index].TypeID)
		if typeID == "" {
			continue
		}
		if _, ok := names[typeID]; ok {
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

func (s *Service) resolveEventStructureTypeNames(ctx context.Context, event *notifications.Event, structures []authnextdb.Structure) map[string]string {
	names := s.resolveStructureTypeNames(ctx, structures)
	if event == nil {
		return names
	}
	for index := range event.StructureReferences {
		reference := &event.StructureReferences[index]
		typeID := strings.TrimSpace(reference.TypeID)
		if typeID == "" {
			continue
		}
		if _, ok := names[typeID]; ok {
			continue
		}
		if name := s.resolveStructureTypeName(ctx, typeID, nil); name != "" {
			names[typeID] = name
		}
	}
	return names
}

func (s *Service) resolveLocation(ctx context.Context, systemID string) universe.SolarSystem {
	location := universe.SolarSystem{ID: systemID}
	if systemID == "" {
		return location
	}
	if s.resolver != nil {
		resolved, err := s.resolver.ResolveSolarSystem(ctx, systemID)
		mergeLocation(&location, &resolved)
		if err != nil {
			s.logger.Warn("alert solar-system enrichment failed", "system_id", systemID, "err", err)
		}
	}
	if location.Name == "" {
		location.Name = s.resolveName(ctx, "solar_system", systemID)
	}
	if location.RegionID != "" && location.RegionName == "" && s.resolver != nil {
		region, err := s.resolver.ResolveRegion(ctx, location.RegionID)
		if err == nil {
			location.RegionName = region.Name
		} else {
			s.logger.Warn("alert region enrichment failed", "region_id", location.RegionID, "err", err)
		}
	}
	return location
}

func mergeLocation(destination, source *universe.SolarSystem) {
	if destination == nil || source == nil {
		return
	}
	if destination.ID == "" {
		destination.ID = source.ID
	}
	if destination.Name == "" {
		destination.Name = source.Name
	}
	if destination.RegionID == "" {
		destination.RegionID = source.RegionID
	}
	if destination.RegionName == "" {
		destination.RegionName = source.RegionName
	}
	if destination.AllianceID == "" {
		destination.AllianceID = source.AllianceID
	}
	if destination.AllianceName == "" {
		destination.AllianceName = source.AllianceName
	}
}

func (s *Service) resolveAlliance(ctx context.Context, event *notifications.Event) (name, iconURL string) {
	if event == nil {
		return "", ""
	}
	if event.AllianceID == "" {
		return event.AllianceName, ""
	}
	if s.resolver == nil {
		if event.AllianceName != "" {
			return event.AllianceName, ""
		}
		return s.resolveName(ctx, "alliance", event.AllianceID), ""
	}
	alliance, err := s.resolver.ResolveAlliance(ctx, event.AllianceID)
	if err != nil {
		s.logger.Warn("alert alliance enrichment failed", "alliance_id", event.AllianceID, "err", err)
		return event.AllianceName, ""
	}
	if alliance.Name == "" {
		alliance.Name = event.AllianceName
	}
	return alliance.Name, alliance.IconURL
}

func (s *Service) enrichAttacker(ctx context.Context, event *notifications.Event) {
	if event == nil || s.resolver == nil {
		return
	}
	s.resolveMissingEntityName(ctx, &event.AttackerCharacterName, event.AttackerCharacterID, "attacker character", s.resolver.ResolveCharacter)
	s.resolveMissingEntityName(ctx, &event.AttackerCorporationName, event.AttackerCorporationID, "attacker corporation", s.resolver.ResolveCorporation)
	s.resolveMissingEntityName(ctx, &event.AttackerAllianceName, event.AttackerAllianceID, "attacker alliance", s.resolver.ResolveAlliance)
}

func (s *Service) resolveMissingEntityName(ctx context.Context, name *string, id, kind string, resolve func(context.Context, string) (universe.Entity, error)) {
	if s == nil || name == nil || strings.TrimSpace(*name) != "" || strings.TrimSpace(id) == "" || resolve == nil {
		return
	}
	entity, err := resolve(ctx, id)
	if err != nil {
		s.logger.Warn("alert entity enrichment failed", "kind", kind, "id", id, "err", err)
		return
	}
	*name = entity.Name
}

func (s *Service) enrichActor(ctx context.Context, event *notifications.Event) {
	if event == nil || s.resolver == nil || event.ActorCharacterID == "" || event.ActorCharacterName != "" {
		return
	}
	character, err := s.resolver.ResolveCharacter(ctx, event.ActorCharacterID)
	if err == nil {
		event.ActorCharacterName = character.Name
	} else {
		s.logger.Warn("alert actor character enrichment failed", "character_id", event.ActorCharacterID, "err", err)
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
	if s.resolver == nil {
		return s.resolveName(ctx, "corporation", corporationID)
	}
	corporation, err := s.resolver.ResolveCorporation(ctx, corporationID)
	if err != nil {
		s.logger.Warn("alert ownership corporation enrichment failed", "corporation_id", corporationID, "err", err)
		return ""
	}
	return corporation.Name
}

func (s *Service) enrichStructures(ctx context.Context, structures []authnextdb.Structure) {
	if s.resolver == nil {
		return
	}
	for index := range structures {
		s.enrichStructure(ctx, &structures[index])
	}
}

func (s *Service) enrichStructure(ctx context.Context, structure *authnextdb.Structure) {
	if s == nil || structure == nil || structure.SystemID == "" || s.resolver == nil {
		return
	}
	location, err := s.resolver.ResolveSolarSystem(ctx, structure.SystemID)
	if err != nil {
		s.logger.Warn("alert structure location enrichment failed", "system_id", structure.SystemID, "err", err)
	}
	if structure.SystemName == nil && location.Name != "" {
		structure.SystemName = new(location.Name)
	}
	if structure.RegionName == nil && location.RegionName != "" {
		structure.RegionName = new(location.RegionName)
	}
	s.enrichStructureCelestial(ctx, structure.PlanetID, &structure.PlanetName, "planet", s.resolver.ResolvePlanet)
	s.enrichStructureCelestial(ctx, structure.MoonID, &structure.MoonName, "moon", s.resolver.ResolveMoon)
}

func (s *Service) enrichStructureCelestial(ctx context.Context, id *string, name **string, kind string, resolve func(context.Context, string) (universe.Entity, error)) {
	if s == nil || id == nil || name == nil || *name != nil || strings.TrimSpace(*id) == "" || resolve == nil {
		return
	}
	entity, err := resolve(ctx, *id)
	if err != nil {
		s.logger.Warn("alert structure celestial enrichment failed", "kind", kind, "id", *id, "err", err)
		return
	}
	if entity.Name != "" {
		*name = new(entity.Name)
	}
}

func (s *Service) resolveName(ctx context.Context, kind, id string) string {
	if id == "" {
		return ""
	}
	database, ok := s.database.(NameDatabase)
	if !ok {
		return ""
	}
	names, err := database.ResolveNames(ctx, kind, []string{id})
	if err != nil {
		s.logger.Warn("alert local name enrichment failed", "kind", kind, "id", id, "err", err)
		return ""
	}
	return names[id]
}

func structureTypeIDForEvent(event *notifications.Event, structures []authnextdb.Structure) string {
	if event == nil || event.NotificationType == "StructuresReinforcementChanged" {
		return ""
	}
	for _, reference := range event.StructureReferences {
		if typeID := strings.TrimSpace(reference.TypeID); typeID != "" {
			return typeID
		}
	}
	if typeID := strings.TrimSpace(event.StructureTypeID); typeID != "" {
		return typeID
	}
	for index := range structures {
		if typeID := strings.TrimSpace(structures[index].TypeID); typeID != "" {
			return typeID
		}
	}
	return ""
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}
