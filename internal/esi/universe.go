package esi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/btnmasher/rex/internal/universe"
)

type allianceResponse struct {
	Name string `json:"name"`
}

type allianceIconsResponse struct {
	PX64X64 string `json:"px64x64"`
}

type solarSystemResponse struct {
	ConstellationID int64  `json:"constellation_id"`
	Name            string `json:"name"`
}

type constellationResponse struct {
	RegionID int64 `json:"region_id"`
}

type regionResponse struct {
	Name string `json:"name"`
}

type namedUniverseResponse struct {
	Name string `json:"name"`
}

// ResolveAlliance returns the alliance name and canonical ESI icon URL.
func (c *Client) ResolveAlliance(ctx context.Context, id string) (universe.Entity, error) {
	endpoint, err := getEntityEndpoint(alliancePath, "/alliances/:id", id)
	if err != nil {
		return universe.Entity{}, err
	}
	endpoint.CacheKey = ""
	details, err := DoJSON[allianceResponse](ctx, c, &Request{Endpoint: endpoint})
	if err != nil {
		return universe.Entity{}, fmt.Errorf("resolve alliance %s: %w", id, err)
	}
	entity := universe.Entity{ID: id, Name: details.Value.Name}
	iconsEndpoint, err := getEntityEndpoint(allianceIconsPath, "/alliances/:id/icons", id)
	if err != nil {
		return entity, nil
	}
	iconsEndpoint.CacheKey = ""
	icons, err := DoJSON[allianceIconsResponse](ctx, c, &Request{Endpoint: iconsEndpoint})
	if err == nil {
		entity.IconURL = icons.Value.PX64X64
	}
	return entity, nil
}

// ResolveSolarSystem returns a solar system and its region from public ESI data.
func (c *Client) ResolveSolarSystem(ctx context.Context, id string) (universe.SolarSystem, error) {
	systemEndpoint, err := getEntityEndpoint(solarSystemPath, "/universe/systems/:id", id)
	if err != nil {
		return universe.SolarSystem{}, err
	}
	systemEndpoint.CacheKey = ""
	system, err := DoJSON[solarSystemResponse](ctx, c, &Request{Endpoint: systemEndpoint})
	if err != nil {
		return universe.SolarSystem{ID: id}, fmt.Errorf("resolve solar system %s: %w", id, err)
	}
	resolved := universe.SolarSystem{ID: id, Name: system.Value.Name}
	constellationID := strconv.FormatInt(system.Value.ConstellationID, 10)
	constellationEndpoint, err := getEntityEndpoint(constellationPath, "/universe/constellations/:id", constellationID)
	if err != nil {
		return resolved, err
	}
	constellationEndpoint.CacheKey = ""
	constellation, err := DoJSON[constellationResponse](ctx, c, &Request{Endpoint: constellationEndpoint})
	if err != nil {
		return resolved, fmt.Errorf("resolve constellation for solar system %s: %w", id, err)
	}
	resolved.RegionID = strconv.FormatInt(constellation.Value.RegionID, 10)
	regionEndpoint, err := getEntityEndpoint(regionPath, "/universe/regions/:id", resolved.RegionID)
	if err != nil {
		return resolved, err
	}
	regionEndpoint.CacheKey = ""
	region, err := DoJSON[regionResponse](ctx, c, &Request{Endpoint: regionEndpoint})
	if err != nil {
		return resolved, fmt.Errorf("resolve region for solar system %s: %w", id, err)
	}
	resolved.RegionName = region.Value.Name
	return resolved, nil
}

// ResolvePlanet returns a planet name from public ESI data.
func (c *Client) ResolvePlanet(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, planetPath, "/universe/planets/:id", id)
}

// ResolveMoon returns a moon name from public ESI data.
func (c *Client) ResolveMoon(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, moonPath, "/universe/moons/:id", id)
}

// ResolveCharacter returns a character name from public ESI data.
func (c *Client) ResolveCharacter(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, characterPath, "/characters/:id", id)
}

// ResolveCorporation returns a corporation name from public ESI data.
func (c *Client) ResolveCorporation(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, corporationPath, "/corporations/:id", id)
}

// ResolveRegion returns a region name from public ESI data.
func (c *Client) ResolveRegion(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, regionPath, "/universe/regions/:id", id)
}

// ResolveStructureType returns a structure type name from public ESI data.
func (c *Client) ResolveStructureType(ctx context.Context, id string) (universe.Entity, error) {
	return c.resolveNamedEntity(ctx, structureTypePath, "/universe/types/:id", id)
}

func (c *Client) resolveNamedEntity(ctx context.Context, template, routeKey, id string) (universe.Entity, error) {
	endpoint, err := getEntityEndpoint(template, routeKey, id)
	if err != nil {
		return universe.Entity{}, err
	}
	endpoint.CacheKey = ""
	response, err := DoJSON[namedUniverseResponse](ctx, c, &Request{Endpoint: endpoint})
	if err != nil {
		return universe.Entity{}, fmt.Errorf("resolve universe entity %s: %w", id, err)
	}
	return universe.Entity{ID: id, Name: response.Value.Name}, nil
}
