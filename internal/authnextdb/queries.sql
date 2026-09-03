-- name: ListNotificationCorporations :many
select corporation_id, name, ticker
from managed_corporations
where is_active = true and (is_member_corporation = true or is_special_purpose = true)
order by corporation_id;

-- name: GetStructuresByIDs :many
select
  cs.structure_id,
  cs.name,
  cs.type_id,
  cs.type_name,
  coalesce(sh.system_id, mg.system_id, cs.system_id) as system_id,
  coalesce(sh.system_name, mg.system_name, cs.system_name) as system_name,
  cs.region_id,
  cs.region_name,
  cs.state,
  cs.low_power,
  coalesce(sh.planet_id, mg.planet_id, '') as planet_id,
  coalesce(sh.planet_name, mg.planet_name) as planet_name,
  mg.moon_id,
  mg.moon_name
from corporation_structures cs
left join structure_skyhooks sh on sh.structure_id = cs.structure_id
left join structure_moon_geographies mg on mg.structure_id = cs.structure_id
where cs.structure_id = any($1::text[]);

-- name: GetSolarSystemDetails :many
select
  systems.solar_system_id,
  systems.solar_system_name,
  systems.region_id,
  regions.region_name,
  sovereignty.alliance_id,
  sovereignty.alliance_name
from universe_eve_solar_systems systems
left join universe_eve_regions regions on regions.region_id = systems.region_id
left join structure_sovereignty_systems sovereignty on sovereignty.system_id = systems.solar_system_id
where systems.solar_system_id = any($1::text[]);

-- name: ResolveCharacterNames :many
select character_id as entity_id, character_name as entity_name
from universe_eve_character_ids where character_id = any($1::text[]);

-- name: ResolveCorporationNames :many
select corporation_id as entity_id, name as entity_name
from managed_corporations
where corporation_id = any($1::text[])
union all
select corporation_id as entity_id, corporation_name as entity_name
from universe_eve_corporation_ids corporations
where corporation_id = any($1::text[])
  and not exists (
    select 1
    from managed_corporations managed
    where managed.corporation_id = corporations.corporation_id
  );

-- name: ResolveAllianceNames :many
select alliance_id as entity_id, alliance_name as entity_name
from universe_eve_alliance_ids where alliance_id = any($1::text[]);

-- name: ResolveSolarSystemNames :many
select solar_system_id as entity_id, solar_system_name as entity_name
from universe_eve_solar_systems where solar_system_id = any($1::text[]);

-- name: ResolveRegionNames :many
select region_id as entity_id, region_name as entity_name
from universe_eve_regions where region_id = any($1::text[]);

-- name: ResolveTypeNames :many
select type_id as entity_id, type_name as entity_name
from universe_eve_inv_types where type_id = any($1::text[]);

-- name: ResolvePlanetNames :many
select planet_id as entity_id, planet_name as entity_name
from universe_eve_planets where planet_id = any($1::text[]);

-- name: ResolveMoonNames :many
select moon_id as entity_id, name as entity_name
from universe_moons where moon_id = any($1::text[]);
