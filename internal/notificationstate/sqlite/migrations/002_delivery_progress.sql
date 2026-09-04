-- +goose Up

alter table notification_retries
  add column delivered_destination_ids_json blob not null default '[]';

-- +goose Down

alter table notification_retries
  drop column delivered_destination_ids_json;
