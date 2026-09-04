-- name: GetCursor :one
select state_json
from notification_cursors
where corporation_id = ?;

-- name: SaveCursor :exec
insert into notification_cursors (corporation_id, state_json, updated_at)
values (?, ?, unixepoch())
on conflict(corporation_id) do update set
  state_json = excluded.state_json,
  updated_at = excluded.updated_at;

-- name: MarkSeen :execresult
insert into notification_seen (notification_id, seen_at)
values (?, unixepoch())
on conflict(notification_id) do nothing;

-- name: InsertPending :exec
insert into notification_retries (
  notification_id,
  corporation_id,
  corporation_name,
  corporation_ticker,
  character_id,
  notification_json,
  destination_ids_json,
  delivered_destination_ids_json,
  failed_destination_ids_json,
  attempts,
  created_at,
  next_retry_at,
  last_error,
  updated_at
)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, unixepoch())
on conflict(notification_id) do nothing;

-- name: ListPending :many
select notification_id, corporation_id, corporation_name, corporation_ticker,
       character_id, notification_json, destination_ids_json,
       delivered_destination_ids_json, failed_destination_ids_json, attempts,
       created_at, next_retry_at, last_error
from notification_retries
where next_retry_at <= ?
order by next_retry_at, notification_id
limit ?;

-- name: ReschedulePending :execrows
update notification_retries
set delivered_destination_ids_json = ?, failed_destination_ids_json = ?, attempts = ?, next_retry_at = ?,
    last_error = ?, updated_at = unixepoch()
where notification_id = ?;

-- name: DeletePending :exec
delete from notification_retries where notification_id = ?;

-- name: PruneSeen :exec
delete from notification_seen
where seen_at < ?
  and not exists (
    select 1 from notification_retries
    where notification_retries.notification_id = notification_seen.notification_id
  );

-- name: InsertAlertHistory :exec
insert into alert_history (
  notification_id,
  notification_type,
  alert_type,
  corporation_id,
  corporation_name,
  corporation_ticker,
  character_id,
  destination_id,
  delivery_status,
  delivery_error,
  dispatched_at,
  raw_notification_json,
  classified_event_json,
  discord_payload_json
)
values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListAlertHistory :many
select id, notification_id, notification_type, alert_type,
       corporation_id, corporation_name, corporation_ticker, character_id,
       destination_id, delivery_status, delivery_error, dispatched_at, raw_notification_json,
       classified_event_json, discord_payload_json
from alert_history
where dispatched_at >= ?
order by dispatched_at desc, id desc
limit ?;

-- name: DeleteAlertHistoryBefore :exec
delete from alert_history where dispatched_at < ?;
