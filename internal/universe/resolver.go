// Package universe resolves EVE identities and locations from the shared database and ESI.
package universe

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
)

const maxCachedEntities = 4096

// Entity is a named EVE entity with an optional image URL.
type Entity struct {
	ID      string
	Name    string
	IconURL string
}

// SolarSystem is a solar system enriched with its region.
type SolarSystem struct {
	ID           string
	Name         string
	RegionID     string
	RegionName   string
	AllianceID   string
	AllianceName string
}

// Database is the database subset used for local universe enrichment.
type Database interface {
	ResolveNames(context.Context, string, []string) (map[string]string, error)
	ResolveSolarSystem(context.Context, string) (SolarSystem, error)
}

// ESI is the public ESI subset used when local universe data is incomplete.
type ESI interface {
	ResolveAlliance(context.Context, string) (Entity, error)
	ResolveCharacter(context.Context, string) (Entity, error)
	ResolveCorporation(context.Context, string) (Entity, error)
	ResolveRegion(context.Context, string) (Entity, error)
	ResolveSolarSystem(context.Context, string) (SolarSystem, error)
	ResolveStructureType(context.Context, string) (Entity, error)
	ResolvePlanet(context.Context, string) (Entity, error)
	ResolveMoon(context.Context, string) (Entity, error)
}

// Resolver is the lookup contract used by alert rendering and other consumers.
type Resolver interface {
	ResolveAlliance(context.Context, string) (Entity, error)
	ResolveCharacter(context.Context, string) (Entity, error)
	ResolveCorporation(context.Context, string) (Entity, error)
	ResolveRegion(context.Context, string) (Entity, error)
	ResolveSolarSystem(context.Context, string) (SolarSystem, error)
	ResolveStructureType(context.Context, string) (Entity, error)
	ResolvePlanet(context.Context, string) (Entity, error)
	ResolveMoon(context.Context, string) (Entity, error)
}

// resolver resolves static identities from a process-lifetime memory cache,
// then the local database, and finally ESI.
type resolver struct {
	database    Database
	esi         ESI
	logger      *slog.Logger
	mu          sync.RWMutex
	entities    map[string]Entity
	entityOrder []string
	systems     map[string]SolarSystem
	systemOrder []string
}

// NewResolver creates a resolver with a process-lifetime identity cache. Either
// source may be nil for tests or reduced deployments.
func NewResolver(database Database, esi ESI) Resolver {
	return NewResolverWithLogger(database, esi, slog.Default())
}

// NewResolverWithLogger creates a resolver using logger for cascade diagnostics.
// Either source may be nil for tests or reduced deployments.
func NewResolverWithLogger(database Database, esi ESI, logger *slog.Logger) Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	return &resolver{
		database: database,
		esi:      esi,
		logger:   logger,
		entities: make(map[string]Entity),
		systems:  make(map[string]SolarSystem),
	}
}

// ResolveAlliance resolves an alliance name locally and falls back to ESI when absent.
func (r *resolver) ResolveAlliance(ctx context.Context, id string) (Entity, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entity{}, nil
	}
	if entity, ok := r.cachedEntity("alliance", id); ok {
		return entity, nil
	}
	entity, found, databaseErr := r.resolveDatabaseEntity(ctx, "alliance", id)
	if found {
		r.cacheEntity("alliance", entity)
		return entity, nil
	}
	if r.esi == nil {
		r.logLookup("alliance", id, "esi", "unavailable", nil)
		if databaseErr != nil {
			return Entity{}, databaseErr
		}
		return Entity{ID: id}, nil
	}
	return r.resolveAllianceFromESI(ctx, id, databaseErr)
}

// ResolveCharacter resolves a character name locally and falls back to ESI.
func (r *resolver) ResolveCharacter(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "character", id, func() (Entity, error) {
		return r.esi.ResolveCharacter(ctx, id)
	})
}

// ResolveCorporation resolves a corporation name locally and falls back to ESI.
func (r *resolver) ResolveCorporation(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "corporation", id, func() (Entity, error) {
		return r.esi.ResolveCorporation(ctx, id)
	})
}

// ResolveRegion resolves a region name locally and falls back to ESI.
func (r *resolver) ResolveRegion(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "region", id, func() (Entity, error) {
		return r.esi.ResolveRegion(ctx, id)
	})
}

// ResolveSolarSystem resolves a system and region, merging local and ESI data.
func (r *resolver) ResolveSolarSystem(ctx context.Context, id string) (SolarSystem, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return SolarSystem{}, nil
	}
	if system, ok := r.cachedSystem(id); ok {
		return system, nil
	}
	local, databaseErr := r.resolveLocalSolarSystem(ctx, id)
	if local.Name != "" && local.RegionName != "" {
		r.cacheSystem(&local)
		return local, nil
	}
	if r.esi == nil {
		r.logLookup("solar_system", id, "esi", "unavailable", nil)
		if databaseErr != nil {
			return local, databaseErr
		}
		return local, nil
	}
	remote, err := r.resolveSolarSystemFromESI(ctx, id)
	if err != nil {
		if databaseErr != nil {
			return local, errors.Join(databaseErr, err)
		}
		return local, err
	}
	mergeSolarSystem(&local, &remote)
	if local.Name != "" && local.RegionName != "" {
		r.cacheSystem(&local)
	}
	return local, nil
}

// ResolveStructureType resolves a structure type name locally and falls back to ESI.
func (r *resolver) ResolveStructureType(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "type", id, func() (Entity, error) {
		return r.esi.ResolveStructureType(ctx, id)
	})
}

// ResolvePlanet resolves a planet through ESI.
func (r *resolver) ResolvePlanet(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "planet", id, func() (Entity, error) {
		return r.esi.ResolvePlanet(ctx, id)
	})
}

// ResolveMoon resolves a moon through ESI.
func (r *resolver) ResolveMoon(ctx context.Context, id string) (Entity, error) {
	return r.resolveNamedEntity(ctx, "moon", id, func() (Entity, error) {
		return r.esi.ResolveMoon(ctx, id)
	})
}

func (r *resolver) resolveNamedEntity(ctx context.Context, kind, id string, resolveESI func() (Entity, error)) (Entity, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entity{}, nil
	}
	if entity, ok := r.cachedEntity(kind, id); ok {
		return entity, nil
	}
	databaseEntity, found, databaseErr := r.resolveDatabaseEntity(ctx, kind, id)
	if found {
		r.cacheEntity(kind, databaseEntity)
		return databaseEntity, nil
	}
	if r.esi == nil {
		r.logLookup(kind, id, "esi", "unavailable", nil)
		if databaseErr != nil {
			return Entity{}, databaseErr
		}
		return Entity{ID: id}, nil
	}
	entity, err := resolveESI()
	switch {
	case err != nil:
		r.logLookup(kind, id, "esi", "error", err)
	case strings.TrimSpace(entity.Name) == "":
		r.logLookup(kind, id, "esi", "miss", nil)
	default:
		r.logLookup(kind, id, "esi", "hit", nil)
	}
	if err != nil && databaseErr != nil {
		return Entity{}, errors.Join(databaseErr, err)
	}
	if err == nil {
		if entity.ID == "" {
			entity.ID = id
		}
		r.cacheEntity(kind, entity)
	}
	return entity, err
}

func (r *resolver) resolveDatabaseEntity(ctx context.Context, kind, id string) (Entity, bool, error) {
	if r.database == nil {
		r.logLookup(kind, id, "database", "unavailable", nil)
		return Entity{}, false, nil
	}
	name, err := r.resolveLocalName(ctx, kind, id)
	if err != nil {
		r.logLookup(kind, id, "database", "error", err)
		return Entity{}, false, err
	}
	if name == "" {
		r.logLookup(kind, id, "database", "miss", nil)
		return Entity{}, false, nil
	}
	r.logLookup(kind, id, "database", "hit", nil)
	return Entity{ID: id, Name: name}, true, nil
}

func (r *resolver) cachedEntity(kind, id string) (Entity, bool) {
	r.mu.RLock()
	entity, ok := r.entities[entityCacheKey(kind, id)]
	r.mu.RUnlock()
	outcome := "miss"
	if ok {
		outcome = "hit"
	}
	r.logLookup(kind, id, "memory", outcome, nil)
	return entity, ok
}

func (r *resolver) cacheEntity(kind string, entity Entity) {
	if strings.TrimSpace(entity.ID) == "" || strings.TrimSpace(entity.Name) == "" {
		return
	}
	key := entityCacheKey(kind, entity.ID)
	r.mu.Lock()
	if r.entities == nil {
		r.entities = make(map[string]Entity)
	}
	if _, exists := r.entities[key]; !exists {
		if len(r.entityOrder) >= maxCachedEntities {
			delete(r.entities, r.entityOrder[0])
			r.entityOrder = r.entityOrder[1:]
		}
		r.entityOrder = append(r.entityOrder, key)
	}
	r.entities[key] = entity
	r.mu.Unlock()
}

func (r *resolver) cachedSystem(id string) (SolarSystem, bool) {
	r.mu.RLock()
	system, ok := r.systems[id]
	r.mu.RUnlock()
	outcome := "miss"
	if ok {
		outcome = "hit"
	}
	r.logLookup("solar_system", id, "memory", outcome, nil)
	return system, ok
}

func (r *resolver) cacheSystem(system *SolarSystem) {
	if system == nil {
		return
	}
	if strings.TrimSpace(system.ID) == "" || strings.TrimSpace(system.Name) == "" || strings.TrimSpace(system.RegionName) == "" {
		return
	}
	r.mu.Lock()
	if r.systems == nil {
		r.systems = make(map[string]SolarSystem)
	}
	if _, exists := r.systems[system.ID]; !exists {
		if len(r.systemOrder) >= maxCachedEntities {
			delete(r.systems, r.systemOrder[0])
			r.systemOrder = r.systemOrder[1:]
		}
		r.systemOrder = append(r.systemOrder, system.ID)
	}
	r.systems[system.ID] = *system
	r.mu.Unlock()
}

func (r *resolver) resolveAllianceFromESI(ctx context.Context, id string, databaseErr error) (Entity, error) {
	entity, err := r.esi.ResolveAlliance(ctx, id)
	if err != nil {
		r.logLookup("alliance", id, "esi", "error", err)
		if databaseErr != nil {
			return Entity{}, errors.Join(databaseErr, err)
		}
		return Entity{}, err
	}
	outcome := "miss"
	if strings.TrimSpace(entity.Name) != "" {
		outcome = "hit"
	}
	r.logLookup("alliance", id, "esi", outcome, nil)
	if entity.ID == "" {
		entity.ID = id
	}
	r.cacheEntity("alliance", entity)
	return entity, nil
}

func (r *resolver) resolveLocalSolarSystem(ctx context.Context, id string) (SolarSystem, error) {
	local := SolarSystem{ID: id}
	if r.database == nil {
		r.logLookup("solar_system", id, "database", "unavailable", nil)
		return local, nil
	}
	resolved, err := r.database.ResolveSolarSystem(ctx, id)
	if err != nil {
		r.logLookup("solar_system", id, "database", "error", err)
		return local, err
	}
	if resolved.ID == "" {
		resolved.ID = id
	}
	r.logLookup("solar_system", id, "database", "hit", nil)
	return resolved, nil
}

func (r *resolver) resolveSolarSystemFromESI(ctx context.Context, id string) (SolarSystem, error) {
	remote, err := r.esi.ResolveSolarSystem(ctx, id)
	if err != nil {
		r.logLookup("solar_system", id, "esi", "error", err)
		return SolarSystem{}, err
	}
	r.logLookup("solar_system", id, "esi", "hit", nil)
	return remote, nil
}

func mergeSolarSystem(local, remote *SolarSystem) {
	if local == nil || remote == nil {
		return
	}
	if local.Name == "" {
		local.Name = remote.Name
	}
	if local.RegionID == "" {
		local.RegionID = remote.RegionID
	}
	if local.RegionName == "" {
		local.RegionName = remote.RegionName
	}
	if local.AllianceID == "" {
		local.AllianceID = remote.AllianceID
	}
	if local.AllianceName == "" {
		local.AllianceName = remote.AllianceName
	}
}

func (r *resolver) logLookup(kind, id, source, outcome string, lookupErr error) {
	if r.logger == nil {
		return
	}
	attrs := []any{
		"kind", kind,
		"id", id,
		"source", source,
		"outcome", outcome,
	}
	if lookupErr != nil {
		attrs = append(attrs, "err", lookupErr)
	}
	r.logger.Debug("universe identity lookup", attrs...)
}

func entityCacheKey(kind, id string) string {
	return kind + ":" + id
}

func (r *resolver) resolveLocalName(ctx context.Context, kind, id string) (string, error) {
	if r.database == nil || strings.TrimSpace(id) == "" {
		return "", nil
	}
	names, err := r.database.ResolveNames(ctx, kind, []string{id})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(names[id]), nil
}
