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
auth-next, and Docker Engine with the Compose v2 plugin if containerized
deployment is desired.

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
| `POLL_INTERVAL` | `1m` | Wall-clock-aligned corporation scheduler interval after the initial startup poll. |
| `POLL_LOOKBEHIND` | `10m` | Maximum age of a notification considered for delivery. Capped at 10 minutes. |
| `HTTP_TIMEOUT` | `20s` | ESI and auth-next HTTP timeout. |
| `AUTH_NEXT_TOKEN_EXPORT_COUNT` | `60` | Number of access tokens requested per corporation, from `1` through `64`. |
| `ALERT_DESTINATIONS_CONFIG` | `alert-destinations.json` | Versioned JSON or YAML destination configuration. |
| `LOG_LEVEL` | `INFO` | `DEBUG`, `INFO`, `WARN`, or `ERROR`. |
| `LOG_PRETTY` | `false` | Use terminal-friendly logs when stderr is a TTY. |
| `LOG_PAYLOADS` | `false` | Include bounded raw payloads in debug logs after routing matches. |
| `JOB_STORE` | `memory` | Generic job store: `memory`, `sqlite`, or `postgres`. |
| `JOB_STORE_SQLITE_PATH` | `rex-jobruntime.sqlite` | SQLite job-store path. |
| `NOTIFICATION_STATE_SQLITE_PATH` | `rex-notification-state.sqlite` | Durable cursor, retry, deduplication, and history database. |

ESI rate-limit coordination is process-local, so run one Rex instance per ESI
client identity. The provider accepts up to 64 token identities per
corporation. The number needed to poll on every scheduler cycle is derived from
the ten-minute identity cooldown and `POLL_INTERVAL`; smaller pools use
smoothed cadence. A token identity is not reused for normal notification
polling within ten minutes.

## Discord Alert Routing

`ALERT_DESTINATIONS_CONFIG` points to a strict version-1 JSON or YAML document.
YAML supports comments. Each destination combines provider-neutral filters and
presentation with a provider-specific `delivery` object:

```json
{
  "version": 1,
  "destinations": [
    {
      "name": "military-alerts",
      "filters": {
        "alertTypes": ["structures.combat"],
        "excludeAlertTypes": ["structures.combat.destroyed"],
        "excludeStructureTypeIDs": ["85230"],
        "includeCorporationIDs": ["123456789"],
        "excludeCorporationIDs": ["987654321"]
      },
      "presentation": {"showEntityIDs": false},
      "delivery": {
        "type": "discord",
        "targets": [
          {"id": "primary", "webhookUrl": "https://discord.com/api/webhooks/replace-me"}
        ],
        "mentionRules": [
          {"alertTypes": ["structures.combat"], "mention": "here"}
        ]
      }
    }
  ]
}
```

- `name` must be unique and is used in retry and history records.
- `filters.alertTypes` accepts canonical groups or individual leaf paths.
- `filters.excludeAlertTypes` accepts leaf paths only and overrides all inclusions.
- `filters.excludeStructureTypeIDs` suppresses matching EVE structure types. Missing type data fails open.
- `filters.includeCorporationIDs` limits delivery to alerts polled through the listed corporations.
- `filters.excludeCorporationIDs` suppresses delivery for the listed polling corporations.
- With neither corporation list, all corporations are accepted; with only one list, it acts as the allowlist or blocklist. When both are set, exclusions take precedence.
- Discord `delivery.targets` requires stable per-destination target IDs. Duplicate webhook URLs across destinations are flattened, so one alert is sent at most once per URL.
- Discord `delivery.mentionRules` accepts `none`, `here`, or `everyone`; the most-specific matching selector wins and the default is no mention.
- Discord sender name and avatar overrides, when needed, are configured under the Discord `delivery` object. `presentation.showEntityIDs` controls entity ID display for that destination.
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
persistent storage. Docker Compose mounts both SQLite databases at `/data` and
sets the job-store path there automatically. For a PostgreSQL job store, set
`JOB_STORE=postgres` and provide the same `DATABASE_URL` used by auth-next.

Terminal alert outcomes are retained in `alert_history` for 30 days. Each row
stores the delivery status, terminal error when applicable, raw notification,
classified event, and exact Discord payload, but never webhook URLs or access
tokens. Failed Discord deliveries are retried durably for at most 30 minutes;
stale or exhausted retry records are then dropped and recorded as failed
outcomes. Recent history can be inspected with:

```sql
select datetime(dispatched_at, 'unixepoch') as dispatched_at,
       notification_id, notification_type, alert_type,
       corporation_name, character_id, destination_id,
       delivery_status, delivery_error,
       raw_notification_json, classified_event_json, discord_payload_json
from alert_history
order by dispatched_at desc, id desc
limit 100;
```

## Commands

```bash
task setup             # Install/check tools and download dependencies.
task verify            # Generate clients, test, vet, and lint.
task ci:checks         # CI gates plus generated-code drift detection.
task build             # Build the service binary.
task run               # Run using .env.
task run:race          # Run with the race detector.
task run:debug:race    # Race detector, pretty logs, payload logging.
task debug:discord     # Send synthetic alerts to configured destinations.
task docker:build      # Build the Docker image.
task docker:up:detach  # Start Docker Compose in the background.
task docker:build:up:detach # Build the image and start Docker Compose.
```

`task debug:discord` invokes `rex --discord-debug`, sends one synthetic event
for every supported alert leaf, and does not connect to PostgreSQL. `task
run:debug:race` forces `LOG_LEVEL=DEBUG`, `LOG_PRETTY=true`, and
`LOG_PAYLOADS=true`.

Rex is a worker process and does not expose HTTP health or readiness endpoints.
Use process supervision and structured logs for operational monitoring.

## Docker And CI

The image does not run migrations during build. On startup, Rex migrates the
selected durable stores before starting workers. The image runs as a non-root
user; Compose additionally makes the root filesystem read-only and persists
SQLite state in the `rex-state` volume.
The destination file mount uses Docker's private SELinux relabel option (`Z`),
which is required on Fedora for the confined container to read a host file.

For local Compose use:

```bash
task docker:build
task docker:up:detach
```

For a published image, set the image tag and restart the container. The same
startup migrations run before workers begin, so a new image applies pending
Rex-owned migrations without a separate migration container:

```bash
export REX_IMAGE=ghcr.io/btnmasher/rex:1.0.0
docker compose pull
docker compose up -d
```

The PostgreSQL database must already exist and be reachable through `.env`.
Run one Rex instance for a given ESI client identity. Do not use a rolling
multi-instance deployment because ESI rate-limit state and notification
coordination are process-local.

Pull requests and pushes to `main` run `task ci:checks` and a cached Docker
build without publishing. Pushing a Go-style `vX.Y.Z` tag runs the same gates
and publishes the exact tag plus `latest` to
`ghcr.io/btnmasher/rex` for amd64 and arm64, with SBOM and provenance
attestations.

## License

Rex and the reusable `jobruntime` module are licensed under the BSD 3-Clause
License. See [LICENSE](LICENSE) and [lib/jobruntime/LICENSE](lib/jobruntime/LICENSE).
