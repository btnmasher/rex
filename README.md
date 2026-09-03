# Rex ESI Notification Service

Rex polls EVE ESI character notifications for corporations managed by the
shared auth-next service and sends supported structure, sovereignty, and other
operational alerts to Discord webhooks.

Rex is a standalone Go service and Docker image. Auth-next owns the PostgreSQL
tables used for corporation, token, universe, and structure data; Rex never
migrates those tables.

Maintainer architecture, package boundaries, runtime flow, and contribution
guidance are documented in [DEVELOPMENT.md](DEVELOPMENT.md).

## Quick Start

Requirements: Go 1.27, [Task](https://taskfile.dev/), PostgreSQL access to
auth-next, and Docker if containerized deployment is desired.

```bash
cp .env.example .env
cp alert-destinations.json.example alert-destinations.json
# Edit .env and alert-destinations.json with real values.
task setup
task run
```

The destination file is required and must contain at least one valid
destination. Startup fails if it is missing, unreadable, empty, or invalid.

## Configuration

The service loads `.env` through the Task commands. Local `.env`, destination,
SQLite, and plan files are ignored by Git.

Required environment variables:

| Variable | Purpose |
| --- | --- |
| `DATABASE_URL` | Auth-next PostgreSQL connection URL. |
| `AUTH_NEXT_TOKEN_EXPORT_BASE_URL` | Auth-next access-token export endpoint. |
| `AUTH_NEXT_TOKEN_EXPORT_BEARER_TOKEN` | Credential for the token export endpoint. |
| `EVE_SSO_CLIENT_ID` | ESI rate-limit identity. |

Common optional settings:

| Variable | Default | Purpose |
| --- | --- | --- |
| `ESI_BASE_URL` | `https://esi.evetech.net` | ESI endpoint. |
| `ESI_COMPATIBILITY_DATE` | `2026-05-19` | ESI compatibility date. |
| `POLL_INTERVAL` | `1m` | Corporation scheduler interval. |
| `POLL_LOOKBEHIND` | `10m` | Maximum age of a notification considered for delivery. Capped at 10 minutes. |
| `HTTP_TIMEOUT` | `20s` | ESI and auth-next HTTP timeout. |
| `ALERT_DESTINATIONS_FILE` | `alert-destinations.json` | Discord destination configuration. |
| `DISCORD_OVERRIDE_SENDER_NAME` | `Rex Alerts` | Optional Discord username override. |
| `DISCORD_OVERRIDE_SENDER_AVATAR_URL` | empty | Optional Discord avatar override. |
| `DISCORD_SHOW_ENTITY_IDS` | `false` | Show hydrated IDs in parentheses; unresolved IDs remain visible as link fallbacks. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, or `ERROR`. |
| `LOG_PRETTY` | `false` | Use terminal-friendly logs when stderr is a TTY. |
| `LOG_PAYLOADS` | `false` | Include bounded raw payloads in debug logs after routing matches. |
| `JOB_STORE` | `memory` | Generic job store: `memory`, `sqlite`, or `postgres`. |
| `JOB_STORE_SQLITE_PATH` | `rex-jobruntime.sqlite` | SQLite job-store path. |
| `NOTIFICATION_STATE_SQLITE_PATH` | `rex-notification-state.sqlite` | Durable cursor, retry, deduplication, and history database. |

ESI rate-limit coordination is process-local, so run one Rex instance per ESI
client identity. Corporations with ten or more usable token identities can be
polled on each scheduler cycle; smaller pools use smoothed cadence. A token
identity is not reused for normal notification polling within ten minutes.

## Discord Alert Routing

`alert-destinations.json` is strict JSON; comments are not supported. Each
destination has:

```json
{
  "name": "military-alerts",
  "webhookUrls": ["https://discord.com/api/webhooks/replace-me"],
  "alertTypes": ["structures.combat"],
  "excludeAlertTypes": ["structures.combat.destroyed"],
  "excludeStructureTypeIDs": ["85230"]
}
```

- `name` must be unique and is used in retry and history records.
- `webhookUrls` accepts one or more Discord webhook URLs.
- `alertTypes` accepts canonical groups or individual leaf paths.
- `excludeAlertTypes` accepts leaf paths only and overrides all inclusions.
- `excludeStructureTypeIDs` suppresses matching EVE structure types. Missing type data fails open.
- Duplicate webhook URLs across destinations are flattened, so one alert is sent at most once per URL.
- `all` or `*` includes every leaf and must be the only included selector.
- Alert selectors are case-insensitive; groups may use either `group` or `group.*`.

The static embed colors are defined by alert meaning and are not configurable:
red for danger and attack events, no-reagent alerts, and ESS main-bank links;
green for gained sovereignty, online, and full-power transitions; blue for
anchoring and informational ownership changes; and yellow for warnings such as
unanchoring, reinforcement changes, vulnerability, fuel, and offline states.

### Supported Alert Selectors

The following is the complete canonical selector matrix. Groups include all
listed descendant leaves.

| Group | Leaf alert paths |
| --- | --- |
| `structures` | All structure leaves below |
| `structures.combat` | `structures.combat.under_attack`, `structures.combat.destroyed` |
| `structures.state` | `structures.state.anchoring`, `structures.state.unanchoring`, `structures.state.reinforced`, `structures.state.online`, `structures.state.low_power`, `structures.state.high_power`, `structures.state.reinforcement_changed`, `structures.state.vulnerable`, `structures.state.services_offline` |
| `structures.ownership` | `structures.ownership.transferred` |
| `structures.resources` | `structures.resources.fuel_alert`, `structures.resources.low_reagents`, `structures.resources.no_reagents` |
| `structures.integrity` | `structures.integrity.lost_shields`, `structures.integrity.lost_armor` |
| `structures.abandonment` | `structures.abandonment.impending_assets_at_risk` |
| `skyhooks` | `skyhooks.under_attack`, `skyhooks.lost_shields`, `skyhooks.destroyed`, `skyhooks.online`, `skyhooks.deployed` |
| `sovereignty` | All sovereignty leaves below |
| `sovereignty.entosis` | `sovereignty.entosis.capture_started`, `sovereignty.entosis.capture_finished`, `sovereignty.entosis.capture_nodes_reinforced` |
| `sovereignty.events` | `sovereignty.events.command_node_event_started`, `sovereignty.events.station_entered_reinforce`, `sovereignty.events.station_exited_reinforce`, `sovereignty.events.structure_reinforced`, `sovereignty.events.structure_destroyed`, `sovereignty.events.all_claim_acquired`, `sovereignty.events.all_claim_lost`, `sovereignty.events.self_destruct_requested`, `sovereignty.events.self_destruct_canceled`, `sovereignty.events.self_destruct_finished`, `sovereignty.events.claimed`, `sovereignty.events.lost` |
| `ess` | `ess.main_bank_link`, `ess.reserve_bank_link` |
| `mercenary_dens` | `mercenary_dens.reinforced`, `mercenary_dens.attacked`, `mercenary_dens.new_mto` |
| `moonmining` | `moonmining.extraction_started`, `moonmining.extraction_canceled`, `moonmining.extraction_finished`, `moonmining.laser_fired`, `moonmining.automatic_fracture` |
| `station_services` | `station_services.enabled`, `station_services.disabled` |
| `starbase` | `starbase.under_attack`, `starbase.resource_alert` |
| `customs_offices` | `customs_offices.attacked`, `customs_offices.reinforced` |

The [example destination file](alert-destinations.json.example) contains
military, logistics, and industry destinations with multiple webhook URLs,
group selections, and structure-type exclusions.

## Alert Details

Rex parses ESI payload metadata into human-readable Discord embeds:

- Corporation, alliance, and character names use EVEWho links, with IDs as fallback links.
- Solar-system and region names use Dotlan links when names are available.
- Structure IDs are resolved globally, without scoping lookups to the polling corporation.
- Structure enrichment includes type, system, region, planet, moon, and structure name where available.
- Payload owner corporations are authoritative; polling-corporation context is only a fallback when owner data is absent.
- Sovereignty events use the hydrated alliance identity and alliance icon.
- Skyhook integrity percentages are bolded and displayed in the embed fields.
- `is_read` is ignored; cursors and notification-ID deduplication determine delivery.
- Structure events are labeled as `Sovereignty Hub` where appropriate.

Names are resolved from the process cache, auth-next universe data, and then
public ESI. Missing enrichment is non-fatal. There is no configurable Skyhook
raid/link alert because no such notification type is supported by the current
ESI notification model.

## Persistence And Migrations

Notification state is always SQLite. The selected job store is independent:

- `memory`: ephemeral, useful for local testing.
- `sqlite`: durable local single-process store.
- `postgres`: durable PostgreSQL store.

Rex applies embedded Goose migrations during startup. SQLite notification-state
and SQLite job-store migrations run for their respective databases. PostgreSQL
job-store migrations run automatically when `JOB_STORE=postgres`; auth-next
tables are never migrated. A migration failure is fatal and prevents workers
from starting.

Explicit migration commands are:

```bash
task migrate:jobstore             # SQLite
task migrate:jobstore:sqlite      # SQLite
task migrate:jobstore:postgres    # PostgreSQL; reads DATABASE_URL from .env
```

The explicit PostgreSQL migration task selects its backend by task name and
requires `DATABASE_URL`; it does not require `JOB_STORE=postgres`.

The equivalent binary modes are `go run ./cmd/rex --migrate --store sqlite`
and `go run ./cmd/rex --migrate --store postgres`. Keep SQLite files on
persistent storage. Docker Compose mounts notification state at `/data`; when
`JOB_STORE=sqlite`, set `JOB_STORE_SQLITE_PATH=/data/rex-jobruntime.sqlite` in
`.env` as well.

Successfully dispatched alerts are retained in `alert_history` for 30 days.
Each row stores the raw notification, classified event, and exact Discord
payload, but never webhook URLs or access tokens. Failed Discord deliveries
are retried durably for at most 30 minutes; stale retry records are then
dropped. Recent history can be inspected with:

```sql
select datetime(dispatched_at, 'unixepoch') as dispatched_at,
       notification_id, notification_type, alert_type,
       corporation_name, character_id, destination_id,
       raw_notification_json, classified_event_json, discord_payload_json
from alert_history
order by dispatched_at desc, id desc
limit 100;
```

## Commands

```bash
task setup             # Install/check tools and download dependencies.
task verify            # Generate clients, test, vet, and lint.
task build             # Build the service binary.
task run               # Run using .env.
task run:race          # Run with the race detector.
task run:debug:race    # Race detector, pretty logs, payload logging.
task debug:discord     # Send synthetic alerts to configured destinations.
task docker:build      # Build the Docker image.
task docker:up:detach  # Start Docker Compose in the background.
```

`task debug:discord` invokes `rex --discord-debug`, sends one synthetic event
for every supported alert leaf, and does not connect to PostgreSQL. `task
run:debug:race` forces `LOG_LEVEL=DEBUG`, `LOG_PRETTY=true`, and
`LOG_PAYLOADS=true`.

Rex is a worker process and does not expose HTTP health or readiness endpoints.
Use process supervision and structured logs for operational monitoring.

## Docker And CI

The image does not run migrations during build. On startup, Rex migrates the
selected durable stores before starting workers. For local Compose use:

```bash
task docker:build
docker compose up -d
```

For a published image, pull the selected image and restart the container; the
same startup migrations run before workers begin. The PostgreSQL database must
already exist and be reachable through `.env`.
Pull requests and pushes to `main` run verification, race tests, and a Docker
build. Pushing a `v*` tag publishes the image to
`ghcr.io/btnmasher/rex`.

## License

Rex and the reusable `jobruntime` module are licensed under the BSD 3-Clause
License. See [LICENSE](LICENSE) and [lib/jobruntime/LICENSE](lib/jobruntime/LICENSE).
