// Package authnextdb contains the repository-owned SQLC boundary for shared auth-next tables.
package authnextdb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/btnmasher/rex/internal/authnextdb/gen"
	"github.com/btnmasher/rex/internal/universe"
)

// Corporation is an eligible corporation configured in the shared database.
type Corporation struct {
	ID     string
	Name   string
	Ticker string
}

// Structure is the persisted structure projection used to enrich notifications.
type Structure struct {
	ID         string
	Name       *string
	TypeID     string
	TypeName   *string
	SystemID   string
	SystemName *string
	RegionID   *string
	RegionName *string
	State      string
	LowPower   bool
	PlanetID   *string
	PlanetName *string
	MoonID     *string
	MoonName   *string
}

// Database exposes domain-shaped queries over the shared PostgreSQL database.
type Database struct {
	queries *gen.Queries
}

// New creates an auth-next database boundary over an existing pool.
func New(pool *pgxpool.Pool) *Database {
	return &Database{queries: gen.New(pool)}
}

// ListNotificationCorporations returns active member and special-purpose corporations.
func (d *Database) ListNotificationCorporations(ctx context.Context) ([]Corporation, error) {
	rows, err := d.queries.ListNotificationCorporations(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Corporation, 0, len(rows))
	for i := range rows {
		row := rows[i]
		out = append(out, Corporation{ID: row.CorporationID, Name: row.Name, Ticker: row.Ticker})
	}
	return out, nil
}

// GetStructuresByIDs returns structure projections for the supplied structure IDs.
func (d *Database) GetStructuresByIDs(ctx context.Context, structureIDs []string) ([]Structure, error) {
	if len(structureIDs) == 0 {
		return nil, nil
	}
	rows, err := d.queries.GetStructuresByIDs(ctx, structureIDs)
	if err != nil {
		return nil, err
	}
	out := make([]Structure, 0, len(rows))
	for i := range rows {
		row := rows[i]
		out = append(out, Structure{
			ID:         row.StructureID,
			Name:       optionalText(row.Name),
			TypeID:     row.TypeID,
			TypeName:   optionalText(row.TypeName),
			SystemID:   row.SystemID,
			SystemName: optionalText(row.SystemName),
			RegionID:   optionalText(row.RegionID),
			RegionName: optionalText(row.RegionName),
			State:      row.State,
			LowPower:   row.LowPower,
			PlanetID:   optionalValue(row.PlanetID),
			PlanetName: optionalText(row.PlanetName),
			MoonID:     optionalText(row.MoonID),
			MoonName:   optionalText(row.MoonName),
		})
	}
	return out, nil
}

// ResolveSolarSystem returns the stored system and region names when available.
func (d *Database) ResolveSolarSystem(ctx context.Context, systemID string) (universe.SolarSystem, error) {
	if systemID == "" {
		return universe.SolarSystem{}, nil
	}
	rows, err := d.queries.GetSolarSystemDetails(ctx, []string{systemID})
	if err != nil {
		return universe.SolarSystem{}, err
	}
	if len(rows) == 0 {
		return universe.SolarSystem{ID: systemID}, nil
	}
	row := rows[0]
	return universe.SolarSystem{
		ID:           row.SolarSystemID,
		Name:         row.SolarSystemName,
		RegionID:     row.RegionID,
		RegionName:   optionalString(row.RegionName),
		AllianceID:   optionalString(row.AllianceID),
		AllianceName: optionalString(row.AllianceName),
	}, nil
}

// ResolveNames resolves IDs against the appropriate auth-next universe projection.
func (d *Database) ResolveNames(ctx context.Context, kind string, ids []string) (map[string]string, error) {
	if len(ids) == 0 {
		return map[string]string{}, nil
	}
	switch kind {
	case "character":
		return d.resolveCharacterNames(ctx, ids)
	case "corporation":
		return d.resolveCorporationNames(ctx, ids)
	case "alliance":
		return d.resolveAllianceNames(ctx, ids)
	case "solar_system":
		return d.resolveSolarSystemNames(ctx, ids)
	case "region":
		return d.resolveRegionNames(ctx, ids)
	case "type":
		return d.resolveTypeNames(ctx, ids)
	case "planet":
		return d.resolvePlanetNames(ctx, ids)
	case "moon":
		return d.resolveMoonNames(ctx, ids)
	default:
		return nil, fmt.Errorf("unsupported universe entity kind %q", kind)
	}
}

func (d *Database) resolveCharacterNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveCharacterNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveCorporationNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveCorporationNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveAllianceNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveAllianceNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveSolarSystemNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveSolarSystemNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveRegionNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveRegionNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveTypeNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveTypeNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolvePlanetNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolvePlanetNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func (d *Database) resolveMoonNames(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := d.queries.ResolveMoonNames(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].EntityID] = rows[i].EntityName
	}
	return out, nil
}

func optionalText(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func optionalValue(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalString(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}
