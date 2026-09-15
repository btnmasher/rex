# Rex Development Guide

This document is for maintainers and contributors. The user-facing setup and
alert configuration guide is in [README.md](README.md).

## Repository Shape

Rex contains two Go modules:

| Path | Module | Role |
| --- | --- | --- |
| `/` | `github.com/btnmasher/rex` | Service, integrations, alerting, and commands. |
| `lib/jobruntime` | `github.com/btnmasher/rex/jobruntime` | Reusable generic job runner and stores. |

Important package ownership:

- `cmd/rex` is the composition root. It owns process signals, startup, migration and debug modes, worker lifecycle, and shutdown ordering.
- `internal/authnextdb` owns SQLC access to auth-next-owned PostgreSQL projections.
- `internal/tokenexport` owns the auth-next token export HTTP boundary, including
  the bounded per-corporation token-count request.
- `internal/token` owns the in-memory corporation token pool and token rotation. It does not query databases or persist tokens.
- `internal/esi` owns ESI transport, typed endpoints, request middleware, ETags, rate limits, and bounded upstream retries.
- `internal/universe` owns memory-cache-first entity and location resolution with optional database and public ESI sources.
- `internal/notifications` owns ESI notification parsing, metadata extraction, classification, canonical alert paths, and safe text handling.
- `internal/routing` owns canonical destination policy, two-phase filtering, and duplicate target validation.
- `internal/enrichment` owns provider-neutral notification data hydration.
- `internal/alerts` owns Discord message rendering, transport adaptation, and history recording.
- `internal/delivery` owns durable admission, asynchronous dispatch, retries, and terminal cleanup.
- `internal/discord` owns the provider-neutral message model and Discord webhook transport.
- `internal/notificationstate` owns cursor, deduplication, pending delivery, and alert-history contracts.
- `internal/poller` owns corporation scheduling, cursor advancement, global notification deduplication, and durable delivery admission.

The `gen/` directories contain SQLC output. Change the owning schema, query, or
SQLC configuration file and regenerate with `task generate`; never edit generated
files directly.

## Runtime Flow

```mermaid
flowchart TD
    A[Application signal context] --> B[Load and validate configuration]
    B --> C[Open auth-next pool]
    C --> D[Apply selected Goose migrations]
    D --> E[Open notification state]
    E --> F[Initial token refresh]
    F --> G[Start refresh scheduler]
    F --> H[Wait for usable token readiness]
    H --> I[Start notification poller]
    I --> J[Fetch, classify, pre-route, and enqueue]
    G --> K[Refresh corporations every 15 minutes]
    J --> L[Delivery worker enriches, post-routes, and dispatches]
    L --> M[Persist cursors, seen IDs, retries, and history]
    A --> N[Cancel workers and wait for shutdown]
    N --> O[Close stores and database pool]
```

The application runs as one process. Startup creates required dependencies
before workers begin. Runtime workers derive from the application context and
are joined before shared resources close. Startup migrations use a
cancellation-independent context so an in-progress migration can finish its
transaction before shutdown proceeds.

The auth-next corporation export requests the count configured by
`AUTH_NEXT_TOKEN_EXPORT_COUNT` (default `60`, maximum `64`). The client validates
the setting and independently bounds accepted response groups to the same
maximum.

The notification pipeline is split from startup and lifecycle orchestration:

```mermaid
flowchart LR
    A[ESI poller] --> B[Fetch and validate notifications]
    B --> C[Classify canonical alert leaf]
    C --> D[Pre-route]
    D -->|No matching targets| E[Mark seen and advance cursor]
    D -->|Matching targets| F[Atomic queue admission]
    F --> G[(Notification state SQLite)]
    G --> H[Delivery worker]
    H --> I[Enrich provider-neutral alert context]
    I --> J[Post-route]
    J -->|No surviving targets| K[Drop queued alert]
    J -->|Surviving targets| L[Destination adapter]
    L --> M[Render and send provider payload]
    M -->|Accepted| N[Record success history]
    M -->|Retryable| O[Reschedule failed targets]
    O --> H
    M -->|Permanent or stale| P[Record failed history]
    N --> Q[Remove queue row]
    P --> Q
    K --> Q
    Q --> G
```

The poller only fetches, validates, classifies, pre-routes, and atomically
persists the notification with its cursor update. It never waits for
enrichment, provider rate limits, or delivery retries. The delivery worker
drains the durable queue independently.

Routing is applied in two stages:

1. **Pre-route:** Uses the polling corporation ID and canonical alert leaf.
   Parent selectors expand to leaves, while excluded leaves take precedence.
   This stage is cheap and runs before queue admission.
2. **Post-route:** Runs after shared enrichment and applies filters requiring
   hydrated data, currently structure-type exclusions and other enriched
   predicates. It operates only on targets persisted by pre-routing.

The queue stores opaque destination target IDs, not provider URLs or rendered
payloads. Destination adapters own provider-specific presentation and
transport failure classification. The generic delivery worker handles success,
retry, stale-drop, and terminal history behavior.

Destination configuration is provider-aware without coupling shared routing to
Discord:

```mermaid
flowchart LR
    A[Versioned destination config] --> B[Shared filters and presentation]
    A --> C[Provider delivery config]
    B --> D[Routing policy]
    C --> E[Provider adapter]
    D --> F[Selected target]
    E --> F
    F --> G[Provider payload]
```

The current provider is Discord. Its targets, sender settings, mention rules,
and webhook payload stay inside the Discord adapter boundary. Future providers
can add their own delivery fields without changing shared filters or the
generic delivery worker.

## Notification Processing

The poller performs an initial evaluation immediately, then evaluates
corporations on wall-clock minute boundaries. Empty token pools are not queued.
Token pools are capped at 64 identities per corporation. The number of
identities required to poll each cycle is derived from the ten-minute identity
cooldown and configured poll interval; smaller pools receive smoothed cadence.
Normal notification polling does not reuse an identity within ten minutes. The
access-token refresh scheduler uses
wall-clock quarter-hour boundaries.

For each character stream, the poller:

1. Selects a usable corporation token and fetches the notification list from ESI.
2. Ignores `is_read`; cursor state and the global notification-ID ledger are authoritative.
3. Sorts eligible rows by notification timestamp and applies the hard ten-minute lookbehind.
4. Marks stale rows and durably admits delivery candidates without advancing past persistence failures.
5. Classifies known notification types into canonical dot-path alert leaves.
6. Pre-routes matching targets without external enrichment and enqueues them atomically with the cursor.
7. The independent delivery worker enriches, post-routes, renders, sends, and retries without blocking polling.

Notification IDs are globally deduplicated. A notification is claimed once,
even if it appears in multiple corporation streams. A pending delivery stores
the raw notification and failed destination IDs so a retry does not refetch the
original row; it reclassifies the stored payload before delivery.

The cursor rules are intentionally conservative:

- Existing streams never deliver a notification older than the ten-minute lookbehind.
- New streams use the same window rather than replaying the entire first ESI response.
- Future upstream timestamps are clamped to current UTC before cursor storage.
- Rows marked stale are considered handled and advance the character cursor.
- Cursor or queue-admission failure stops processing before later rows can be skipped; delivery failures remain owned by the queue worker.

## Alert Taxonomy And Routing

Canonical alert definitions live in
[`internal/notifications/notifications.go`](internal/notifications/notifications.go).
Each supported ESI type maps to one leaf path and, where appropriate, one or
more parent groups. Configuration expands groups to leaves, then applies
destination-scoped leaf exclusions. Exclusions always win over inclusion.

Routing is evaluated in this order:

1. Match the polling corporation against the destination's corporation filter.
2. Match the classified leaf against the destination's compiled selectors.
3. Apply the destination's excluded leaf paths.
4. Expand each matching destination into opaque delivery targets.
5. Apply excluded structure type IDs using payload type data first and enriched structure data second.
6. Flatten duplicate webhook URLs after post-routing filters so one notification produces at most one delivery per surviving URL. Pre-routing does not flatten URLs because shared URLs may have different post-routing filters.

The first surviving target wins when the same URL appears in multiple matching
destinations. Its target ID is retained for retry and alert-history records.
Duplicate opaque target IDs are rejected during configuration compilation.
Corporation filters use the polling corporation ID in the queued alert
envelope, not a payload owner corporation. An empty include list means all
corporations; a
non-empty include list limits delivery to those IDs. Exclusions always win,
including when an ID appears in both lists.
Semantic embed colors are static in `internal/alerts`; destination configuration
does not control colors.

When adding or changing a leaf:

1. Add or update the ESI type mapping, display name, group membership, parser, and renderer behavior.
2. Add classifier and routing tests, including group inclusion and leaf exclusion.
3. Add a synthetic debug fixture in `cmd/rex/discord_debug.go`.
4. Update the selector matrix in [README.md](README.md) and the example destination file when configuration changes.
5. Run `task verify` and `task test:race`.

## Enrichment

`internal/universe` resolves entities through this cascade:

1. Process-lifetime memory cache.
2. Auth-next database projection, when a database is configured.
3. Public ESI universe lookup.

The resolver logs cache hits and misses at debug level. If an entity is absent
from local cache and the database, the ESI request is forced rather than sent
with an ETag that could produce an unusable `304 Not Modified`. ETags are used
only when a cached response body is available. Resolver failures are best
effort for alert rendering and must not discard an otherwise deliverable alert.

Structure lookup is keyed by globally unique structure ID and is never scoped
to the corporation that polled the notification. Structure records may be
enriched with system, region, planet, moon, and type data. The ESI payload's
owner corporation is authoritative; polling-corporation context is only a
fallback when owner data is absent.

Moon-mining extraction-finished, manual laser-fired, and automatic-fracture
notifications may include `oreVolumeByType`. Classification preserves the
bounded ore volumes, enrichment resolves their type names, and Discord renders
the resulting percentages as a compact list. Fracture descriptions state that
the extraction is ready for harvesting.

## Persistence And Migrations

Notification state and generic job state are separate:

| Store | Tables | Migration owner |
| --- | --- | --- |
| Notification-state SQLite | Cursors, seen IDs, delivery retries, alert history. | Rex, `internal/notificationstate/sqlite`. |
| Job-store SQLite | Generic runs, work items, checkpoints, and retry groups. | `lib/jobruntime/jobstore/sqlite`. |
| Job-store PostgreSQL | The same generic job tables for durable production jobs. | `lib/jobruntime/jobstore/postgres`. |
| Auth-next PostgreSQL | Corporation, token, universe, and structure projections. | Auth-next, never Rex. |

Rex uses embedded Goose migrations and the `goose_db_version` table. Each
database has one initial migration stream owned by its package. PostgreSQL job
migrations are applied automatically when `JOB_STORE=postgres`; SQLite job
migrations are not run against PostgreSQL, and auth-next tables are never
migrated.

Store constructors apply migrations automatically. The explicit commands use
the same application migration code:

```bash
task migrate:jobstore
task migrate:jobstore:sqlite
task migrate:jobstore:postgres
```

The explicit PostgreSQL task selects the backend by task name and requires
`DATABASE_URL` from the environment or root `.env`; it does not require
`JOB_STORE=postgres`. The `JOB_STORE` setting only controls which job-store
backend the running service constructs and therefore which automatic job-store
migrations it applies.

Migration work must complete atomically before workers start. A migration
failure is fatal. If schema changes require SQLC output, update the source SQL,
run `task generate`, inspect generated changes, then run `task verify`.

## Delivery And Recovery

Discord delivery is bounded and context-aware. Each webhook call makes one
HTTP request; `429` and server errors return a retryable result instead of
sleeping in a polling worker. The durable pending queue applies bounded
exponential retry, honors Discord's reported delay up to the 30-minute safety
cap, and drops stale alerts after 30 minutes. A bounded per-webhook gate
serializes requests and suppresses requests during an active rate-limit
cooldown.

Alert history records one outcome per destination and is retained for 30 days.
It contains the raw notification, classified event, exact Discord payload, and
terminal delivery error when applicable, but never webhook URLs or access
tokens. Successful deliveries are recorded immediately; transient failures
remain in the retry queue and failed history is recorded only when retry is
exhausted or becomes stale. History is pruned at startup and after insertions.

The generic job runtime isolates failures at two levels:

- A claimed-item panic is logged and converted into the normal bounded retry or final-failure path.
- A lifecycle panic is recovered, logged with type and stack, returned as an error, and best-effort marked as a failed run.

Recovery operations derive a bounded context from the caller context without
discarding context values. Nil stores, requests, views, and optional resolvers
must be checked before dereference. Every claimed item must reach retry or
terminal state before a batch completes.

## Concurrency Rules

Rex is deployed as a single instance. ESI rate-limit buckets and entity caches
are process-local. Do not add multi-instance assumptions to notification-state
deduplication or retry draining without introducing an explicit distributed
lease or shared coordination store.

Use context-aware waits for every timer, semaphore, network request, database
operation, and worker handoff. New goroutines must have a clear owner, a
shutdown signal, and a wait path. The application waits for both workers before
closing the database or stores.

Per-corporation token rotation uses keyed serialization. Refresh requests are
coalesced per corporation, fetch auth-next data without holding that rotation
lock, and publish the complete replacement snapshot under the provider mutex.
A canceled rotation waiter must release its registry reference, and an idle
keyed lock must be removed so lock state cannot grow without bound.

## Development Commands

Run from the repository root:

```bash
task setup
task generate
task verify
task ci:checks
task test:race
task build
task docker:build:up:detach
task run:debug:race
task debug:discord
```

`task verify` regenerates SQLC clients, checks formatting, runs tests, runs
`go vet`, and runs `golangci-lint` in both modules. `task ci:checks` adds race
tests and fails if generation or formatting changes the checkout. `task ci`
also builds the Docker image. Use `task test:race` for concurrency-sensitive
changes. The Discord debug command sends synthetic events through the
configured routing file without connecting to PostgreSQL.

For focused jobruntime work:

```bash
go -C lib/jobruntime test ./...
go -C lib/jobruntime vet ./...
go -C lib/jobruntime test -race ./...
```

Generated clients are checked into the repository. Do not hand-edit files
under `gen/`.

## Docker And CI

The Dockerfile builds a CGO-free static service image as a non-root user.
Migration files are embedded in the binary and are not executed during image
build. A new image applies pending migrations at startup before workers begin.
The base Compose configuration uses the Docker-managed `rex-state` volume at
`/data` for both SQLite stores, makes the container root filesystem read-only,
and grants write access only to that volume. A machine-local
`docker-compose.override.yml` may replace that volume with a `.data/` bind
mount for development; that host directory must be provisioned with write
access for the image user (UID `65532`). Production deployments should use the
base Compose file or provision a persistent host mount with that ownership
themselves.

The destination configuration bind mount uses Docker's private SELinux relabel
option for hosts that require it. The image accepts `VERSION`, `REVISION`, and
`CREATED` build arguments for OCI metadata. `task docker:logs` follows the container
stdout/stderr stream, colorizes the Compose prefix and JSON syntax for the
terminal, and appends a raw copy to `.logs/rex.log`; it follows only new log
entries rather than replaying retained container history. Rex itself continues
to emit logs to stdout/stderr. The local copy rotates at 10 MiB with five
retained files, while Compose's container log driver has the same bound. The
task requires `jq` on the host.

The CI workflow runs `task ci:checks` and a cached Buildx build for pull
requests and pushes to `main`. The release workflow accepts Go-style `vX.Y.Z`
tags, runs the same gates, and publishes the exact tag plus `latest` to GHCR
using the workflow's built-in `GITHUB_TOKEN`. Release images target amd64 and
arm64 and include SBOM and provenance attestations. The release workflow is
the only workflow with `packages: write` permission.

## Security Checklist

- Keep `.env` and the configured JSON/YAML destination file untracked.
- Never log auth-next bearer credentials, access tokens, or webhook URLs.
- Keep payload logging disabled unless actively troubleshooting.
- Keep the production Compose `rex-state` volume or equivalent persistent mount
  on durable storage.
- Validate new external URLs and bound response, payload, and history sizes.
- Preserve secret redaction when adding structured log attributes or durable fields.
