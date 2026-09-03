# Rex ESI Notification Service

Rex is a standalone Go service packaged as a Docker container. It polls EVE ESI
character notifications for active member and special-purpose corporations in
the shared auth-next PostgreSQL database, then delivers structure-related
alerts to configured Discord webhooks.

The canonical Go module path is `github.com/btnmasher/rex`.

The service requires Go 1.27 for local builds and uses the Go 1.27 toolchain in
the Docker build. The reusable job runner and typed work-item APIs use generic
types and functions.

## Architecture

```text
Task/Docker process
  -> cmd/rex
    -> poller
      -> authnextdb SQLC boundary
      -> auth-next access-token export and in-memory token pool
      -> ESI notifications client
    -> notification classifier
      -> local alert routing configuration
      -> Discord delivery port
```

The scheduler evaluates each corporation once per minute, while each
corporation's actual poll cadence is spread across its active token identities.
Corporations with ten or more usable tokens can poll on every scheduler cycle;
smoothed cadence is applied only to smaller token pools.
Failed deliveries enter durable SQLite retry state and are drained in bounded
batches. An ESI authentication failure triggers a scoped auth-next access-token
export for that corporation. Normal notification requests do not reuse a token
identity within ten minutes.

Corporation cursors, globally seen notification IDs, pending delivery
payloads, and successfully dispatched alert history are stored in the dedicated
SQLite notification-state database. ESI
notification IDs are globally unique, so the first corporation to observe an ID
claims it and later corporations suppress it. Cared-for alerts are claimed in
the same transaction that records their retry payload before delivery, so a
crash cannot lose an alert between deduplication and queueing. Successful
delivery removes the pending row; partial destination failures retain only the
failed destination IDs, and the retry drain processes at most a bounded batch
per poll. Seen IDs are retained for one hour, including IDs with pending retries.
Retry records are discarded after 30 minutes from their first queue insertion,
even if delivery continues failing.
The configured lookbehind is a hard maximum age for every fetched notification;
older rows are marked stale and advance the character cursor without delivery.
New corporation or character streams use the same lookbehind window when their
initial cursor is created.

## Repository Layout

- `cmd/rex/`: service composition root and graceful shutdown lifecycle.
- `internal/authnextdb/`: SQLC queries over auth-next-owned tables.
- `internal/esi/`: reusable ESI transport and typed endpoints.
- `internal/universe/`: memory-cache-first, database-second identity/location enrichment with public ESI fallback.
- `internal/notifications/`: classification and safe text handling.
- `internal/alerts/`: destination resolution and message templates.
- `internal/discord/`: provider-neutral delivery port and webhook adapter.
- `internal/notificationstate/`: cursor-store interface and SQLite persistence.
- `lib/jobruntime/`: reusable generic job runner with memory, SQLite, and SQLC PostgreSQL stores.
- `lib/jobruntime/jobstore/postgres/migrations/`: rex-owned durable job tables only.

Auth-next tables are externally owned. Rex does not migrate them and does not
depend on an auth-next checkout at build or runtime.

## Configuration

Copy `.env.example` to `.env` and provide real values. Create
`alert-destinations.json` from
`alert-destinations.json.example` and replace the webhook placeholders. Each
destination accepts one or more URLs in `webhookUrls`. The
`task run` command loads the root-level `.env` automatically; both local files
are ignored by Git:

- `DATABASE_URL`: shared PostgreSQL connection URL.
- `AUTH_NEXT_TOKEN_EXPORT_BASE_URL`: auth-next base URL for the internal access-token export endpoint.
- `AUTH_NEXT_TOKEN_EXPORT_BEARER_TOKEN`: bearer credential for that endpoint.
- `EVE_SSO_CLIENT_ID`: EVE application identifier used for ESI rate-limit coordination.
- `ESI_BASE_URL`, `ESI_COMPATIBILITY_DATE`: optional ESI settings.
- `POLL_INTERVAL`, `POLL_LOOKBEHIND`, `HTTP_TIMEOUT`: duration overrides. Polling defaults to one minute, and the notification lookbehind is capped at ten minutes.
- `ALERT_DESTINATIONS_FILE`: path to the local JSON file containing Discord webhook destinations and alert category mappings. Defaults to `alert-destinations.json`.
- `DISCORD_OVERRIDE_SENDER_NAME`: optional sender-name override applied to every Discord webhook message. It defaults to `Rex Alerts`; the webhook's configured name is used only when the override is empty in a direct library integration.
- `DISCORD_OVERRIDE_SENDER_AVATAR_URL`: optional avatar URL override applied to every Discord webhook message. Leave unset to use each webhook's configured avatar.
- `DISCORD_SHOW_ENTITY_IDS`: optional boolean, default `false`; show hydrated EVE entity IDs in parentheses alongside linked names.
- `LOG_PRETTY`: optional boolean, default `false`; use compact styled logs on interactive terminals. Non-terminal output remains JSON.
- `LOG_PAYLOADS`: optional boolean, default `false`; include bounded raw notification payloads in debug logs after an alert matches at least one Discord destination. Keep disabled unless needed for local troubleshooting.
- `JOB_STORE`: `memory`, `sqlite`, or `postgres` for generic job-runtime composition.
- `JOB_STORE_SQLITE_PATH`: SQLite database path when `JOB_STORE=sqlite`; the schema is initialized automatically.
- `NOTIFICATION_STATE_SQLITE_PATH`: SQLite database path for durable notification cursors, deduplication, retries, and alert history.
- `LOG_LEVEL`: logging settings. Use `LOG_LEVEL=DEBUG`
  for redacted ESI request, token rotation, notification processing, alert
  routing, and Discord delivery diagnostics.

ESI route limits are coordinated as moving-window buckets across concurrent
requests. The limiter is process-local and the service should run as one active
replica per ESI client identity unless a shared `RateLimitStore` is supplied at
composition time. Discord webhook destinations are restricted to supported
Discord hosts and must satisfy Discord embed limits.

`JOB_STORE` and notification state are separate concerns. The job store tracks
job runtime metadata; notification state tracks ESI cursors and delivered IDs.
The service automatically initializes the notification-state SQLite schema and,
when selected, the SQLite job-store schema with embedded Goose migrations.
Applied versions are stored in `goose_db_version`. PostgreSQL job-store
migrations run automatically only when `JOB_STORE=postgres` and the PostgreSQL
store is constructed.
`task migrate:jobstore` applies SQLite migrations by default;
`task migrate:jobstore:sqlite` and `task migrate:jobstore:postgres` select the
backend explicitly, with the PostgreSQL task requiring `JOB_STORE=postgres`.
Docker Compose mounts the SQLite
database at `/data` so it survives container recreation.

The Docker image embeds the service binary and both SQLite migration sets. It
does not migrate during image build. On each startup, the selected durable
store applies pending Goose migrations before workers start; PostgreSQL job
store migrations run only when `JOB_STORE=postgres`. Deploying a new image is
therefore: pull the image, restart the container, and let startup migration
complete. A migration failure exits non-zero instead of starting a partially
upgraded service. The notification-state SQLite file and any SQLite job-store
file must be kept on persistent storage.

## Alert Routing

### Alert Types

The destination `alertTypes` field accepts canonical dot-path groups and leaf
alert types. The table below is the exhaustive supported selector taxonomy. A
group expands to all descendant leaves. A group may be written
as either `skyhooks` or `skyhooks.*`; both normalize to the same selector.
Exclusions accept individual leaf paths only and always take precedence.

| Group path | Leaf alert paths |
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

`ALERT_DESTINATIONS_FILE` contains the many-to-many mapping from alert
groups and leaf types to Discord webhook URLs. Destination names must be unique
and are used as the stable retry identifiers; when a destination has multiple
webhooks, each URL receives a private stable retry target ID. Alert paths are case-insensitive
and must be known canonical values from the table above. Identical webhook URLs
across destination configurations are flattened at delivery time, so one
notification produces at most one message per URL; the first matching
configuration supplies the stable retry/history target for overlapping selectors.
A destination may list multiple groups or leaf types, and multiple destinations
may list the same alert. The optional `excludeAlertTypes` field accepts only leaf
alert types.
Exclusions take precedence over every included group or leaf, so a destination
can include a parent group while suppressing selected sub-alerts. The special
`all` or `*` value includes every leaf type and may be combined with
`excludeAlertTypes`, but not with other `alertTypes` values.
The default semantic colors are red
for structure and skyhook attacks, entosis, sovereignty loss, destruction,
and other danger alerts; green for gained sovereignty; blue for structure
anchoring; and yellow for ESS, fuel, and structure-services-offline alerts.

The optional `excludeStructureTypeIDs` field accepts up to 128 positive EVE
structure type IDs. A destination is suppressed when the notification payload
contains a matching `structureTypeID`; if the payload omits it, enriched
structure rows are checked instead. Missing type information fails open so an
alert is not silently discarded. For example, `81080` is the EVE structure
type ID for Orbital Skyhooks.

Notifications are classified from typed ESI payload fields. Known
metadata-only notifications receive a human description instead of a raw
metadata dump. Rex uses payload names when present, then resolves names and
locations from auth-next universe tables, and finally falls back to public ESI
lookups; payload names remain the final identity fallback. Solar-system
enrichment follows the system's constellation to its region. Structure lookups
join the tracked structure row with the auth-next Skyhook and moon-geography
sidecars by globally unique structure ID, independent of the polling
corporation. This exposes structure name, type, system, region, planet, and
moon where available.

The current ESI notification enum has no dedicated Skyhook raid or link event.
Skyhook attack, lost-shields/reinforcement, destruction, online, and deployment
states are supported; an explicit Skyhook raid/link notification is not
invented as a configurable alert type.

Sovereignty and alliance-originated notifications use the hydrated alliance
identity and canonical ESI alliance icon as the embed author. Attack
notifications show attacker character and corporation context when the ESI
payload provides IDs; Rex resolves those IDs from auth-next universe tables
first and public ESI second. For structure notifications, the ESI payload's
owner corporation is authoritative; when a new structure is not yet present in
auth-next, Rex uses the polling corporation context. ESI `is_read` is
ignored: the durable cursor and global notification-ID ledger determine
whether an alert is new.

Sovereignty structure events are presented as `Sovereignty Hub` events rather
than exposing the underlying ESI station/structure terminology.

Example:

```dotenv
ALERT_DESTINATIONS_FILE=alert-destinations.json
DISCORD_OVERRIDE_SENDER_NAME=Rex Alerts
DISCORD_OVERRIDE_SENDER_AVATAR_URL=https://images.evetech.net/types/35832/render?size=64
DISCORD_SHOW_ENTITY_IDS=false
```

The corresponding destination entries look like this:

```json
[
  {
    "name": "structure-alerts",
    "webhookUrls": ["https://discord.com/api/webhooks/replace-me"],
    "alertTypes": ["structures.combat"]
  },
  {
    "name": "sovereignty-alerts",
    "webhookUrls": ["https://discord.com/api/webhooks/replace-me"],
    "alertTypes": ["sovereignty.events"],
    "excludeAlertTypes": ["sovereignty.events.command_node_event_started"],
    "excludeStructureTypeIDs": ["81080"]
  }
]
```

Use `"all"` or `"*"` as the only item in a destination's `alertTypes` array to
receive every leaf alert. Unmapped alerts are ignored after classification and
deduplication. The webhook URL is the Discord channel destination; no channel
ID is configured separately.

The destination file is mandatory for the daemon and must contain at least one
valid destination. An absent, unreadable, invalid, or empty destination file is
a fatal startup configuration error; Rex never starts polling with alerts
silently disabled.

Every delivered notification is a Discord embed. Structure notifications use
the notification structure type ID, falling back to the first enriched
structure's type ID, to set the embed thumbnail to
`https://images.evetech.net/types/{type_id}/render?size=64`. This is an
implementation detail and is not configurable. Rex parses ESI structure
metadata such as owner corporation, structure type, solar system, region,
planet, moon, integrity, and remaining duration into named fields instead of
exposing the raw metadata payload. Times use Discord timestamp markup alongside
a UTC 24-hour `EVE Time` display.
Structure type and region names use the process-lifetime resolver cache, then
the shared auth-next universe projections, and finally public ESI lookups;
the notification's `structureTypeID` is retained as a fallback. The same type
ID drives the EVE image thumbnail.
Starbase notifications use `starbase.under_attack` for POS attacks and
`starbase.resource_alert` for POS resource shortages. Tower attack payloads
provide aggressor IDs and shield, armor, and hull value fields; resource
payloads provide the owning corporation/alliance and requested resource type
IDs and quantities.
Customs-office notifications use `customs_offices.attacked` for orbital attacks
and `customs_offices.reinforced` when an orbital enters reinforcement. Their
payloads provide the solar system, planet, customs-office type, aggressor IDs,
and, for reinforcement notifications, the reinforcement exit timestamp.
Character, corporation, and alliance names are rendered as EVEWho links, with
IDs used as the fallback when a name is unavailable. Set
`DISCORD_SHOW_ENTITY_IDS=true` to also retain hydrated IDs in the label.
Solar-system and region names link to Dotlan using their hydrated names.
Skyhook shield, armor, and hull percentages are emphasized in bold.
Ownership-transfer alerts include separate previous-owner and new-owner
corporation fields, with names hydrated from the shared universe data.
The structure database is optional for alert rendering; unresolved enrichment
falls back to linked IDs rather than failing delivery.

### Alert History

After Discord accepts a delivery, Rex writes one row to the `alert_history`
table in the notification-state SQLite database for that destination. Each row
contains the notification ID and type, classified alert category, polling
corporation and character, destination name, the decoded ESI notification JSON,
the enriched classified-event JSON, and the exact Discord message JSON sent to
the webhook. Webhook URLs and access tokens are never stored.

History is retained for 30 days. Rex prunes expired rows during SQLite startup
and while recording new deliveries. Inspect recent history with a SQLite
client, for example:

```sql
select datetime(dispatched_at, 'unixepoch') as dispatched_at,
       notification_id, notification_type, alert_type,
       corporation_name, character_id, destination_id,
       raw_notification_json, classified_event_json, discord_payload_json
from alert_history
order by dispatched_at desc, id desc
limit 100;
```

Only deliveries accepted by Discord are recorded. If one destination fails,
successful destinations are retained in history while only the failed
destinations enter the bounded retry queue.

Access tokens are held only in process memory. The `access-token-refresh` job
lists eligible corporations and fetches each corporation's token set every
fifteen minutes with bounded concurrency. An ESI authentication failure forces
a corporation-scoped access-token list refresh.

Corporations with an empty token set are not queued for ESI notification
polling. Corporations with configured token records that are expired or
otherwise unusable remain observable through the poller's diagnostic logging
and are retried by the refresh job.

At startup, Rex runs one refresh immediately. Notification polling remains
disabled until that run successfully discovers corporations and caches at least
one usable access token. If auth-next is unavailable or returns an error,
periodic refresh retry remains active, but ESI polling does not start with an
empty token pool.

## Commands

Task is the primary command interface:

```bash
task setup
task generate
task verify
task build
task run
task run:race
task run:debug:race
task debug:discord
task docker:build
task docker:up:detach
```

Use `task run:race` for local daemon testing with Go's race detector. It loads
`.env` and uses the configured job store. Set `JOB_STORE=memory` in `.env` when
you want an ephemeral local run. The race detector is intended for
single-process local runs, not production containers.

Use `task run:debug:race` for the same race-enabled daemon run with verbose,
terminal-friendly logging. It loads `.env` but intentionally overrides
`LOG_LEVEL=DEBUG`, `LOG_PRETTY=true`, and `LOG_PAYLOADS=true`.

Use `task debug:discord` to send one realistic synthetic example for every
supported alert leaf through the configured Discord destinations,
without starting the daemon or connecting to PostgreSQL. This invokes
`rex --discord-debug`, which uses
the configured `ALERT_DESTINATIONS_FILE`, `DISCORD_OVERRIDE_SENDER_NAME`,
`DISCORD_OVERRIDE_SENDER_AVATAR_URL`, and `DISCORD_SHOW_ENTITY_IDS` from the
root `.env`; the task forces `LOG_PRETTY=true` for terminal-friendly output.

Migration tasks invoke the same base binary. `task migrate:jobstore` defaults
to SQLite; use `task migrate:jobstore:postgres` for PostgreSQL after setting
`JOB_STORE=postgres`. The service also applies the selected store migrations
automatically during startup. The equivalent direct modes are
`go run ./cmd/rex --migrate --store sqlite`,
`go run ./cmd/rex --migrate --store postgres`, and
`go run ./cmd/rex --discord-debug`.

Run `task --list` for the complete backend-only command tree. Direct Go commands
run from the repository root. Docker Compose expects an existing `.env` and
shared PostgreSQL database; it creates the SQLite state file in its named
`/data` volume but does not provision PostgreSQL or any frontend/sidecar
services.

## Continuous Integration

Pull requests and pushes to `main` run `task verify`, race-enabled tests, and a
Docker build without publishing. Pushing a `v*` tag publishes the image to
`ghcr.io/btnmasher/rex` with the release tag and `latest` tags. The release
workflow uses GitHub's built-in `GITHUB_TOKEN`; no registry credential is
stored in the repository.

## Operations

Rex is a worker process and does not expose an HTTP health or readiness server.
Use process supervision and structured logs to observe startup, refresh, polling,
delivery, and shutdown state. A failed startup configuration or dependency setup
causes the process to exit non-zero.
