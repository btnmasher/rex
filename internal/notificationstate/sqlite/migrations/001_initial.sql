-- +goose Up

create table if not exists notification_cursors (
  corporation_id text primary key,
  state_json blob not null,
  updated_at integer not null default (unixepoch())
);

create table if not exists notification_seen (
  notification_id integer primary key,
  seen_at integer not null
);

create index if not exists notification_seen_seen_at_idx on notification_seen (seen_at);

create table if not exists notification_retries (
  notification_id integer primary key,
  corporation_id text not null,
  corporation_name text not null,
  corporation_ticker text not null,
  character_id text not null,
  notification_json blob not null,
  destination_ids_json blob not null,
  failed_destination_ids_json blob not null,
  attempts integer not null default 0,
  created_at integer not null,
  next_retry_at integer not null,
  last_error text not null default '',
  updated_at integer not null
);

create index if not exists notification_retries_due_idx
  on notification_retries(next_retry_at, notification_id);

create table if not exists alert_history (
  id integer primary key autoincrement,
  notification_id integer not null,
  notification_type text not null,
  alert_type text not null,
  corporation_id text not null,
  corporation_name text not null,
  corporation_ticker text not null,
  character_id text not null,
  destination_id text not null,
  delivery_status text not null,
  delivery_error text not null default '',
  dispatched_at integer not null,
  raw_notification_json blob not null,
  classified_event_json blob not null,
  discord_payload_json blob not null
);

create index if not exists alert_history_dispatched_at_idx
  on alert_history(dispatched_at, id);

create index if not exists alert_history_notification_id_idx
  on alert_history(notification_id, dispatched_at);

-- +goose Down

drop table if exists alert_history;
drop table if exists notification_retries;
drop table if exists notification_seen;
drop table if exists notification_cursors;
