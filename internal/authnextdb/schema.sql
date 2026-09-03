create table managed_corporations (
  corporation_id text primary key,
  name varchar(255) not null,
  ticker varchar(10) not null,
  is_active boolean not null default true,
  is_member_corporation boolean not null default false,
  is_special_purpose boolean not null default false
);

create table corporation_structures (
  id uuid primary key,
  corporation_id text not null,
  structure_id text not null,
  name text,
  type_id text not null,
  type_name text,
  system_id text not null,
  system_name text,
  region_id text,
  region_name text,
  state text not null,
  low_power boolean not null default false
);

create table structure_skyhooks (
  structure_id text primary key,
  corporation_id text not null,
  planet_id text,
  planet_name text,
  system_id text,
  system_name text,
  type_id text not null,
  state text not null,
  is_active boolean not null default false
);

create table structure_moon_geographies (
  structure_id text primary key,
  corporation_id text not null,
  moon_id text not null,
  moon_name text,
  planet_id text not null,
  planet_name text,
  system_id text not null,
  system_name text
);

create table structure_sovereignty_systems (
  system_id text primary key,
  alliance_id text,
  alliance_name text
);

create table universe_eve_character_ids (
  character_id text primary key,
  character_name text not null
);

create table universe_eve_corporation_ids (
  corporation_id text primary key,
  corporation_name text not null,
  ticker text
);

create table universe_eve_alliance_ids (
  alliance_id text primary key,
  alliance_name text not null,
  ticker text
);

create table universe_eve_solar_systems (
  solar_system_id text primary key,
  solar_system_name text not null,
  region_id text not null,
  constellation_id text not null
);

create table universe_eve_regions (
  region_id text primary key,
  region_name text not null
);

create table universe_eve_planets (
  planet_id text primary key,
  planet_name text not null,
  solar_system_id text not null,
  celestial_index integer not null,
  type_id text
);

create table universe_moons (
  id bigint primary key,
  name text not null,
  moon_id text not null,
  planet_id text not null,
  solar_system_id text not null
);

create table universe_eve_inv_types (
  type_id text primary key,
  type_name text not null
);
