create table job_runs (
  run_id text primary key,
  job_id text not null,
  job_kind text not null,
  slot_ts timestamptz not null,
  status text not null,
  trigger_type text not null,
  trigger_meta jsonb not null default '{}'::jsonb,
  started_at timestamptz not null default now(),
  updated_at timestamptz not null default now(),
  heartbeat_at timestamptz,
  checkpoint_namespace text not null default '',
  checkpoint_value bytea,
  stats_json jsonb not null default '{}'::jsonb,
  error text
);

create unique index job_runs_job_slot on job_runs(job_id, slot_ts);
create unique index job_runs_one_running_per_kind on job_runs(job_kind) where status = 'RUNNING';
create index job_runs_status_updated on job_runs(status, updated_at desc);

create table job_work_items (
  run_id text not null references job_runs(run_id),
  item_id bigint not null,
  state text not null,
  attempt_count int not null default 0,
  next_retry_at timestamptz,
  payload_json jsonb,
  result_json jsonb,
  last_error text,
  retry_group_id text,
  updated_at timestamptz not null default now(),
  primary key (run_id, item_id)
);

create index job_work_items_state_retry on job_work_items(run_id, state, next_retry_at);
create index job_work_items_retry_group on job_work_items(retry_group_id, state);

create table job_events (
  id bigserial primary key,
  run_id text not null references job_runs(run_id),
  ts timestamptz not null default now(),
  level text not null,
  kind text not null,
  meta jsonb not null default '{}'::jsonb
);

create table job_retry_groups (
  retry_group_id text primary key,
  job_id text not null,
  job_kind text not null,
  created_at timestamptz not null default now(),
  created_by text,
  source_run_id text,
  status text not null
);
