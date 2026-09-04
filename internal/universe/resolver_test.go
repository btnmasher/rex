package universe

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

type resolverDatabase struct {
	names     map[string]string
	err       error
	system    SolarSystem
	systemErr error
}

func (d *resolverDatabase) ResolveNames(_ context.Context, _ string, ids []string) (map[string]string, error) {
	if d.err != nil {
		return nil, d.err
	}
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		if name := d.names[id]; name != "" {
			result[id] = name
		}
	}
	return result, nil
}

func (d *resolverDatabase) ResolveSolarSystem(context.Context, string) (SolarSystem, error) {
	return d.system, d.systemErr
}

type resolverESI struct {
	corporation        Entity
	corporationCalls   *int
	region             Entity
	regionCalls        *int
	structureType      Entity
	structureTypeCalls *int
	systemResponse     *SolarSystem
	systemErr          error
}

func (resolverESI) ResolveAlliance(context.Context, string) (Entity, error) {
	return Entity{}, errors.New("unexpected alliance lookup")
}

func (e *resolverESI) ResolveCorporation(context.Context, string) (Entity, error) {
	if e.corporationCalls != nil {
		*e.corporationCalls++
	}
	return e.corporation, nil
}

func (e *resolverESI) ResolveRegion(context.Context, string) (Entity, error) {
	if e.regionCalls != nil {
		*e.regionCalls++
	}
	return e.region, nil
}

func (resolverESI) ResolveCharacter(context.Context, string) (Entity, error) {
	return Entity{}, errors.New("unexpected character lookup")
}

func (e *resolverESI) ResolveSolarSystem(context.Context, string) (SolarSystem, error) {
	if e.systemResponse != nil || e.systemErr != nil {
		if e.systemResponse == nil {
			return SolarSystem{}, e.systemErr
		}
		return *e.systemResponse, e.systemErr
	}
	return SolarSystem{}, errors.New("unexpected system lookup")
}

func (e *resolverESI) ResolveStructureType(context.Context, string) (Entity, error) {
	if e.structureTypeCalls != nil {
		*e.structureTypeCalls++
	}
	return e.structureType, nil
}

func (resolverESI) ResolvePlanet(context.Context, string) (Entity, error) {
	return Entity{}, errors.New("unexpected planet lookup")
}

func (resolverESI) ResolveMoon(context.Context, string) (Entity, error) {
	return Entity{}, errors.New("unexpected moon lookup")
}

func TestResolverRejectsNilContexts(t *testing.T) {
	resolver := NewResolver(nil, nil)
	var nilContext context.Context
	if _, err := resolver.ResolveAlliance(nilContext, "100"); err == nil {
		t.Fatal("expected nil alliance context to fail")
	}
	if _, err := resolver.ResolveCharacter(nilContext, "100"); err == nil {
		t.Fatal("expected nil character context to fail")
	}
	if _, err := resolver.ResolveCorporation(nilContext, "100"); err == nil {
		t.Fatal("expected nil corporation context to fail")
	}
	if _, err := resolver.ResolveRegion(nilContext, "100"); err == nil {
		t.Fatal("expected nil region context to fail")
	}
	if _, err := resolver.ResolveSolarSystem(nilContext, "100"); err == nil {
		t.Fatal("expected nil solar system context to fail")
	}
	if _, err := resolver.ResolveStructureType(nilContext, "100"); err == nil {
		t.Fatal("expected nil structure type context to fail")
	}
	if _, err := resolver.ResolvePlanet(nilContext, "100"); err == nil {
		t.Fatal("expected nil planet context to fail")
	}
	if _, err := resolver.ResolveMoon(nilContext, "100"); err == nil {
		t.Fatal("expected nil moon context to fail")
	}
}

func TestResolveCorporationPrefersDatabaseName(t *testing.T) {
	resolver := NewResolver(
		&resolverDatabase{names: map[string]string{"100": "Managed Corporation"}},
		&resolverESI{corporation: Entity{ID: "100", Name: "ESI Corporation"}},
	)

	got, err := resolver.ResolveCorporation(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Managed Corporation" {
		t.Fatalf("corporation name = %q, want managed database name", got.Name)
	}
}

func TestResolveCorporationFallsBackToESIOnDatabaseError(t *testing.T) {
	resolver := NewResolver(
		&resolverDatabase{err: errors.New("database unavailable")},
		&resolverESI{corporation: Entity{ID: "100", Name: "ESI Corporation"}},
	)

	got, err := resolver.ResolveCorporation(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ESI Corporation" {
		t.Fatalf("corporation name = %q, want ESI fallback", got.Name)
	}
}

func TestResolveCorporationWorksWithoutDatabase(t *testing.T) {
	resolver := NewResolver(nil, &resolverESI{corporation: Entity{ID: "100", Name: "ESI Corporation"}})

	got, err := resolver.ResolveCorporation(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "ESI Corporation" {
		t.Fatalf("corporation name = %q, want ESI fallback", got.Name)
	}
}

func TestResolveSolarSystemPreservesPartialDataOnESIFailure(t *testing.T) {
	sentinel := errors.New("region service unavailable")
	resolver := NewResolver(
		&resolverDatabase{system: SolarSystem{ID: "300", Name: "Local System"}},
		&resolverESI{systemResponse: &SolarSystem{RegionID: "10000015", RegionName: "Pure Blind"}, systemErr: sentinel},
	)

	got, err := resolver.ResolveSolarSystem(context.Background(), "300")
	if !errors.Is(err, sentinel) {
		t.Fatalf("resolve error = %v, want sentinel", err)
	}
	if got.Name != "Local System" || got.RegionID != "10000015" || got.RegionName != "Pure Blind" {
		t.Fatalf("partial solar system = %+v", got)
	}
}

func TestResolverCapsIdentityCache(t *testing.T) {
	resolver := NewResolver(nil, nil).(*resolver)
	for index := range maxCachedEntities + 1 {
		resolver.cacheEntity("corporation", Entity{
			ID:   strconv.Itoa(index),
			Name: "Corporation " + strconv.Itoa(index),
		})
	}
	resolver.mu.RLock()
	defer resolver.mu.RUnlock()
	if len(resolver.entities) != maxCachedEntities || len(resolver.entityOrder) != maxCachedEntities {
		t.Fatalf("identity cache size = %d/%d, want %d", len(resolver.entities), len(resolver.entityOrder), maxCachedEntities)
	}
}

func TestResolveCorporationCachesSuccessfulFallback(t *testing.T) {
	var calls int
	resolver := NewResolver(
		&resolverDatabase{},
		&resolverESI{
			corporation:      Entity{ID: "100", Name: "ESI Corporation"},
			corporationCalls: &calls,
		},
	)

	for range 2 {
		got, err := resolver.ResolveCorporation(context.Background(), "100")
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "ESI Corporation" {
			t.Fatalf("corporation name = %q, want ESI Corporation", got.Name)
		}
	}
	if calls != 1 {
		t.Fatalf("ESI corporation calls = %d, want 1", calls)
	}
}

func TestResolveCorporationLogsCascade(t *testing.T) {
	var output bytes.Buffer
	resolver := NewResolverWithLogger(
		&resolverDatabase{},
		&resolverESI{corporation: Entity{ID: "100", Name: "ESI Corporation"}},
		slog.New(slog.NewTextHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})),
	)

	if _, err := resolver.ResolveCorporation(context.Background(), "100"); err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	if _, err := resolver.ResolveCorporation(context.Background(), "100"); err != nil {
		t.Fatalf("cached resolution: %v", err)
	}
	logOutput := output.String()
	for _, want := range []string{
		"source=memory outcome=miss",
		"source=database outcome=miss",
		"source=esi outcome=hit",
		"source=memory outcome=hit",
	} {
		if !strings.Contains(logOutput, want) {
			t.Fatalf("log output missing %q: %q", want, logOutput)
		}
	}
}

func TestResolveRegionAndStructureTypeCacheESIFallbacks(t *testing.T) {
	var regionCalls, structureTypeCalls int
	resolver := NewResolver(
		&resolverDatabase{},
		&resolverESI{
			region:             Entity{ID: "10000015", Name: "Pure Blind"},
			regionCalls:        &regionCalls,
			structureType:      Entity{ID: "35834", Name: "Metenox Moon Drill"},
			structureTypeCalls: &structureTypeCalls,
		},
	)

	region, err := resolver.ResolveRegion(context.Background(), "10000015")
	if err != nil {
		t.Fatalf("resolve region: %v", err)
	}
	if region.Name != "Pure Blind" {
		t.Fatalf("region name = %q, want Pure Blind", region.Name)
	}
	typeEntity, err := resolver.ResolveStructureType(context.Background(), "35834")
	if err != nil {
		t.Fatalf("resolve structure type: %v", err)
	}
	if typeEntity.Name != "Metenox Moon Drill" {
		t.Fatalf("structure type name = %q, want Metenox Moon Drill", typeEntity.Name)
	}

	if _, err := resolver.ResolveRegion(context.Background(), "10000015"); err != nil {
		t.Fatalf("resolve cached region: %v", err)
	}
	if _, err := resolver.ResolveStructureType(context.Background(), "35834"); err != nil {
		t.Fatalf("resolve cached structure type: %v", err)
	}
	if regionCalls != 1 || structureTypeCalls != 1 {
		t.Fatalf("ESI calls = region %d, structure type %d; want one each", regionCalls, structureTypeCalls)
	}
}

func TestResolveRegionAndStructureTypePreferDatabase(t *testing.T) {
	resolver := NewResolver(
		&resolverDatabase{names: map[string]string{
			"10000015": "Database Region",
			"35834":    "Database Structure Type",
		}},
		&resolverESI{
			region:        Entity{ID: "10000015", Name: "ESI Region"},
			structureType: Entity{ID: "35834", Name: "ESI Structure Type"},
		},
	)

	region, err := resolver.ResolveRegion(context.Background(), "10000015")
	if err != nil {
		t.Fatalf("resolve region: %v", err)
	}
	typeEntity, err := resolver.ResolveStructureType(context.Background(), "35834")
	if err != nil {
		t.Fatalf("resolve structure type: %v", err)
	}
	if region.Name != "Database Region" || typeEntity.Name != "Database Structure Type" {
		t.Fatalf("database precedence failed: region=%#v type=%#v", region, typeEntity)
	}
}
