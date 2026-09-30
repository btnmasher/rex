-- +goose Up

create table if not exists notification_skips (
  id integer primary key autoincrement,
  notification_id integer not null,
  notification_type text not null,
  sender_id integer not null,
  sender_type text not null,
  notification_timestamp integer not null,
  corporation_id text not null,
  corporation_name text not null,
  corporation_ticker text not null,
  character_id text not null,
  reason text not null,
  skipped_at integer not null,
  raw_notification_json blob not null
);

create index if not exists notification_skips_skipped_at_idx
  on notification_skips(skipped_at, id);

-- +goose Down

drop table if exists notification_skips;
