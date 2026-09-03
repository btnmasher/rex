-- name: GetRunningRunByKind :one
select run_id, slot_ts
from job_runs
where job_kind = $1 and status = 'RUNNING'
order by started_at desc
limit 1;

-- name: ReclaimStaleRunByKind :one
update job_runs
set updated_at = now(), heartbeat_at = now(), error = null
where job_kind = $1
  and status = 'RUNNING'
  and coalesce(heartbeat_at, updated_at) < now() - interval '5 minutes'
returning run_id, slot_ts;

-- name: InsertRun :one
insert into job_runs(run_id, job_id, job_kind, slot_ts, trigger_type, trigger_meta, status)
values ($1, $2, $3, $4, $5, $6::jsonb, 'RUNNING')
on conflict (run_id) do nothing
returning run_id, slot_ts;

-- name: CompleteRun :execrows
update job_runs
set status = 'COMPLETED', updated_at = now(), heartbeat_at = now(),
    stats_json = coalesce($2::jsonb, stats_json)
where run_id = $1 and status = 'RUNNING';

-- name: FailRun :execrows
update job_runs
set status = 'FAILED', updated_at = now(), heartbeat_at = now(), error = $2
where run_id = $1 and status = 'RUNNING';

-- name: CancelRun :execrows
update job_runs
set status = 'CANCELED', updated_at = now(), heartbeat_at = now(), error = $2
where run_id = $1 and status = 'RUNNING';

-- name: HeartbeatRun :execrows
update job_runs set heartbeat_at = now(), updated_at = now() where run_id = $1 and status = 'RUNNING';

-- name: GetRunCheckpoint :one
select checkpoint_namespace, checkpoint_value from job_runs where run_id = $1;

-- name: SaveRunCheckpoint :execrows
update job_runs
set checkpoint_namespace = $2, checkpoint_value = $3, updated_at = now()
where run_id = $1 and status = 'RUNNING';

-- name: InsertWorkItem :execrows
insert into job_work_items(run_id, item_id, state, payload_json, result_json)
values ($1, $2, 'READY', $3::jsonb, $4::jsonb)
on conflict (run_id, item_id) do update
set item_id = job_work_items.item_id
where job_work_items.payload_json is not distinct from excluded.payload_json;

-- name: ClaimItems :many
with candidates as (
  select source.run_id, source.item_id
  from job_work_items source
  where source.run_id = $1
    and (
      (source.state in ('READY', 'FAILED_RETRY') and (source.next_retry_at is null or source.next_retry_at <= now()))
      or (source.state = 'INFLIGHT' and source.updated_at < now() - interval '15 minutes')
    )
    and ($2 = '' or source.retry_group_id = $2)
  order by source.item_id
  limit $3
  for update skip locked
)
update job_work_items as item
set state = 'INFLIGHT', attempt_count = item.attempt_count + 1, updated_at = now()
from candidates
where item.run_id = candidates.run_id and item.item_id = candidates.item_id
returning item.run_id, item.item_id, item.attempt_count, item.payload_json, item.result_json, item.retry_group_id;

-- name: CompleteItem :execrows
update job_work_items
set result_json = coalesce($4::jsonb, result_json), state = 'DONE', next_retry_at = null,
    last_error = null, retry_group_id = null, updated_at = now()
where run_id = $1 and item_id = $2 and attempt_count = $3 and state = 'INFLIGHT';

-- name: RetryItem :execrows
update job_work_items
set state = 'FAILED_RETRY', next_retry_at = $4, last_error = $5, updated_at = now()
where run_id = $1 and item_id = $2 and attempt_count = $3 and state = 'INFLIGHT';

-- name: FailItem :execrows
update job_work_items
set state = 'FAILED_FINAL', last_error = $4, updated_at = now()
where run_id = $1 and item_id = $2 and attempt_count = $3 and state = 'INFLIGHT';

-- name: GetPendingItems :one
select coalesce(ceil(extract(epoch from (min(next_retry_at) - now())))::bigint, 0) as retry_delay_seconds,
       count(*)::int as pending_count,
       count(*) filter (where state = 'FAILED_RETRY')::int as retry_count
from job_work_items
where run_id = $1
  and state in ('READY', 'FAILED_RETRY', 'INFLIGHT')
  and ($2 = '' or retry_group_id = $2);

-- name: CountItemsByState :many
select state, count(*)::int from job_work_items where run_id = $1 group by state;

-- name: InsertEvent :exec
insert into job_events(run_id, level, kind, meta) values ($1, $2, $3, $4::jsonb);

-- name: InsertRetryGroup :exec
insert into job_retry_groups(retry_group_id, job_id, job_kind, created_by, source_run_id, status)
values ($1, $2, $3, $4, $5, 'OPEN');

-- name: TagRetryGroupItems :execrows
update job_work_items
set retry_group_id = $1, state = 'READY', next_retry_at = null, last_error = null, updated_at = now()
where run_id = $2 and state in ('FAILED_FINAL', 'FAILED_RETRY') and item_id = any($3::bigint[]);

-- name: TagRetryGroupAllItems :execrows
update job_work_items
set retry_group_id = $1, state = 'READY', next_retry_at = null, last_error = null, updated_at = now()
where run_id = $2 and state in ('FAILED_FINAL', 'FAILED_RETRY');

-- name: GetRetryGroupForUpdate :one
select retry_group_id, job_id, job_kind, source_run_id, status
from job_retry_groups where retry_group_id = $1 for update;

-- name: MarkRetryGroupRunning :execrows
update job_retry_groups set status = 'RUNNING' where retry_group_id = $1 and status = 'OPEN';

-- name: ResumeRetryRun :execrows
update job_runs as run
set status = 'RUNNING', updated_at = now(), heartbeat_at = now(), error = null
where run.run_id = $1 and not exists (
  select 1 from job_runs other
  where other.job_kind = $2 and other.status = 'RUNNING' and other.run_id <> $1
);

-- name: CompleteRetryGroup :execrows
update job_retry_groups set status = 'DONE' where retry_group_id = $1;

-- name: ReopenRetryGroup :execrows
update job_retry_groups set status = 'OPEN' where retry_group_id = $1;

-- name: GetRunStatus :one
select run_id, job_id, job_kind, status, trigger_type, started_at, updated_at,
       ''::text as current_step, 0::int as progress_done, 0::int as progress_total,
       coalesce(error, '') as error
from job_runs where run_id = $1;

-- name: ListRunStatuses :many
select run_id, job_id, job_kind, status, trigger_type, started_at, updated_at,
       ''::text as current_step, 0::int as progress_done, 0::int as progress_total,
       coalesce(error, '') as error
from job_runs
where ($1::text = '' or status = $1)
order by started_at desc limit $2;
