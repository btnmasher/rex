-- +goose Up

create table if not exists job_runs (
  run_id text primary key,
  job_id text not null,
  job_kind text not null,
  slot_ts integer not null,
  status text not null,
  trigger_type text not null,
  trigger_meta blob not null default '{}',
  started_at integer not null default (unixepoch()),
  updated_at integer not null default (unixepoch()),
  heartbeat_at integer,
  checkpoint_namespace text not null default '',
  checkpoint_value blob,
  stats_json blob not null default '{}',
  error text
);

create unique index if not exists job_runs_job_slot on job_runs(job_id, slot_ts);
create unique index if not exists job_runs_one_running_per_kind on job_runs(job_kind) where status = 'RUNNING';
create index if not exists job_runs_status_updated on job_runs(status, updated_at desc);

create table if not exists job_work_items (
  run_id text not null references job_runs(run_id),
  item_id integer not null,
  state text not null,
  attempt_count integer not null default 0,
  next_retry_at integer,
  payload_json blob,
  result_json blob,
  last_error text,
  retry_group_id text,
  updated_at integer not null default (unixepoch()),
  primary key (run_id, item_id)
);

create index if not exists job_work_items_state_retry on job_work_items(run_id, state, next_retry_at);
create index if not exists job_work_items_retry_group on job_work_items(retry_group_id, state);

create table if not exists job_events (
  id integer primary key autoincrement,
  run_id text not null references job_runs(run_id),
  ts integer not null default (unixepoch()),
  level text not null,
  kind text not null,
  meta blob not null default '{}'
);

create index if not exists job_events_run_ts on job_events(run_id, ts desc);

create table if not exists job_retry_groups (
  retry_group_id text primary key,
  job_id text not null,
  job_kind text not null,
  created_at integer not null default (unixepoch()),
  created_by text,
  source_run_id text,
  status text not null
);

create index if not exists job_retry_groups_status on job_retry_groups(status, created_at desc);

-- +goose Down

drop table if exists job_retry_groups;
drop table if exists job_events;
drop table if exists job_work_items;
drop table if exists job_runs;
