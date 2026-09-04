# AGENTS Ruleset

This document is an execution policy for coding agents and contributors.

## Priority Order

1. Follow explicit user instructions for the current task.
2. Follow this `AGENTS.md` policy.
3. Preserve existing project conventions in nearby code.
4. Optimize for correctness, maintainability, and minimal-risk changes.

## Command & Workflow Policy

### Task-First Execution (MUST)

- Use `task` as the primary command interface.
- Prefer `task` over ad-hoc `go`, `bun`, or `docker` commands.
- If a repeated workflow is missing from `Taskfile.yml`, add a task.
- SQLC query functions and models must be generated, never manually edited. Change
  the owning `queries.sql`, `schema.sql`, or `sqlc.yaml`, then run `task generate`
  or the appropriate scoped generator task before testing or reviewing the diff.
- Treat every `gen/` directory containing SQLC output as read-only generated
  output; inspect generated changes for correctness but do not hand-edit them.

### Standard Commands

- Setup: `task setup`
- Generate clients: `task generate`
- Local service: `task run`
- Build: `task build`
- Backend checks: `task verify`
- CI gates: `task ci:checks`
- Docker image: `task docker:build`
- Durable job tables only: `task migrate:jobstore`

## Safety & Change Hygiene

- Never commit secrets or credentials.
- Keep diffs minimal and directly related to the task.
- Update docs and task commands when operational behavior changes.

## Go Navigation & Refactoring Rules (mcp-gopls)

Always prioritize `mcp-gopls` tools for Go analysis.

- Searching symbols: use `search_workspace_symbols`.
- Finding references: use `find_references`.
- Definition lookup: use `go_to_definition`.
- Renaming symbols: use `rename_symbol`.
- Inspection: use `get_hover_info` and `check_diagnostics`.
- Module cleanup: use `run_go_mod_tidy` when imports/modules change.
- Test/quality/security:
  - `run_go_test`
  - `analyze_coverage`
  - `run_govulncheck`

### Verification & Deprecation Rules (MUST)

- Before suggesting creation of a method, verify existence with `get_hover_info`.
- Scan hover docs for `Deprecated:`.
- If deprecated:
  - Do not suggest the deprecated symbol by default.
  - Surface the recommended alternative when available.
- If user explicitly asks for deprecated usage:
  - Warn first and explain tradeoffs.
- If hover is ambiguous, cross-check deprecation with `check_diagnostics` (SA1019).

### Reasoning Loop (MUST)

1. If user asks to use method X, run `get_hover_info` on the receiver type.
2. If missing/symbol-not-found, only then suggest creating it.
3. If present, use the real signature from tool output.

### Fail-Safe

- Never assume method availability from naming conventions.
- Always confirm with LSP context first.

## Go Style Rules

1. Keep a logically coupled assignment and its immediate validation together with no blank line, especially `value, err := ...` followed by `if err != nil`.
2. Use blank lines between logical phases of a function, such as input validation, dependency calls, transformation, persistence, and return; do not add blank lines between statements in the same phase.
3. Do not shadow error variables in the same function scope.
4. Prefer early returns to reduce nesting.
5. Prefer modular package boundaries by domain (for example `internal/esi`, `internal/intel`, `internal/sde`, `internal/map`, `internal/auth`).
6. Use repository-style interfaces for persistence access.
7. Use constants for repeated strings; place package constants in `constants.go`.
8. Create `errors.go` per package for package-level errors; wrap with `%w` for `errors.Is/As`.
9. Use const groups for enum-like parameters with exported, prefixed names (for example `AuthProviderEVE`, `AuthProviderSSO`).
10. Check pointers and interfaces for nil before dereferencing; keep nil guards at the boundary of renderers and helpers that accept pointers.
11. File structure order:
   1. Package-level vars
   2. Types
   3. Constructors
   4. Methods
   5. Exported functions
   6. Unexported functions

## Go API Design Rules

- Keep function and method signatures focused on values the implementation actually needs; do not thread unrelated configuration, endpoint parameters, status values, or request metadata through every call merely because one consumer uses them.
- Prefer a small purpose-built request or context object when several related values form one stable concept, rather than growing positional parameter lists.
- Avoid indirection that only forwards a call or hides a single implementation detail. Add an interface, adapter, or helper when it defines a real package boundary, supports multiple implementations, isolates an external dependency, or materially improves testability.
- Keep optional behavior explicit and localized. Do not expose parameters or interfaces solely to support speculative future use.
- Review new public APIs for invalid combinations of arguments, unclear ownership, unnecessary coupling, and whether the simplest correct caller experience is being preserved.

## GoDoc Rules

- Every exported identifier (type, function, method, const group, var) must have a GoDoc comment.
- The comment must start with the identifier name.
- Comments must be full sentences and explain behavior.
- Include non-obvious side effects when relevant.
- Do not restate the name without adding meaning.
- Write for readers unfamiliar with the local implementation.

# DOX framework

- DOX is highly performant AGENTS.md hierarchy installed here
- Agent must follow DOX instructions across any edits

## Core Contract

- AGENTS.md files are binding work contracts for their subtrees
- Work products, source materials, instructions, records, assets, and durable docs must stay understandable from the nearest applicable AGENTS.md plus every parent AGENTS.md above it
- `DEVELOPMENT.md` is the maintainer guide for repository architecture, package ownership, runtime invariants, and development workflows; keep it aligned with implementation changes.

## Read Before Editing

1. Read the root AGENTS.md
2. Identify every file or folder you expect to touch
3. Walk from the repository root to each target path
4. Read every AGENTS.md found along each route
5. If a parent AGENTS.md lists a child AGENTS.md whose scope contains the path, read that child and continue from there
6. Use the nearest AGENTS.md as the local contract and parent docs for repo-wide rules
7. If docs conflict, the closer doc controls local work details, but no child doc may weaken DOX

Do not rely on memory. Re-read the applicable DOX chain in the current session before editing.

## Update After Editing

Every meaningful change requires a DOX pass before the task is done.

Update the closest owning AGENTS.md when a change affects:

- purpose, scope, ownership, or responsibilities
- durable structure, contracts, workflows, or operating rules
- required inputs, outputs, permissions, constraints, side effects, or artifacts
- user preferences about behavior, communication, process, organization, or quality
- AGENTS.md creation, deletion, move, rename, or index contents

Update parent docs when parent-level structure, ownership, workflow, or child index changes. Update child docs when parent changes alter local rules. Remove stale or contradictory text immediately. Small edits that do not change behavior or contracts may leave docs unchanged, but the DOX pass still must happen.

## Hierarchy

- Root AGENTS.md is the DOX rail: project-wide instructions, global preferences, durable workflow rules, and the top-level Child DOX Index
- Child AGENTS.md files own domain-specific instructions and their own Child DOX Index
- Each parent explains what its direct children cover and what stays owned by the parent
- The closer a doc is to the work, the more specific and practical it must be

## Child Doc Shape

- Create a child AGENTS.md when a folder becomes a durable boundary with its own purpose, rules, responsibilities, workflow, materials, or quality standards
- Work Guidance must reflect the current standards of the project or user instructions; if there are no specific standards or instructions yet, leave it empty
- Verification must reflect an existing check; if no verification framework exists yet, leave it empty and update it when one exists

Default section order:
- Purpose
- Ownership
- Local Contracts
- Work Guidance
- Verification
- Child DOX Index

## Style

- Keep docs concise, current, and operational
- Document stable contracts, not diary entries
- Put broad rules in parent docs and concrete details in child docs
- Prefer direct bullets with explicit names
- Do not duplicate rules across many files unless each scope needs a local version
- Delete stale notes instead of explaining history
- Trim obvious statements, repeated rules, misplaced detail, and warnings for risks that no longer exist

## Linting (Go)
We use `golangci-lint` for backend linting.

Run directly:
```bash
golangci-lint run ./...
```

### Automated Checks
1. Error variable shadowing is disallowed (`govet` with `check-shadowing`).
2. Complex nesting and high cognitive complexity should be avoided (`nestif`, `gocognit`).
3. Error wrapping/handling correctness (`errorlint`, `errcheck`).
4. Misuse of contexts and loop variables (`noctx`, `copyloopvar`).
5. Unused or suspicious code (`staticcheck`, `ineffassign`, `unparam`, `unconvert`, `wastedassign`).
6. Spelling and lint suppression hygiene (`misspell`, `nolintlint`).

### Manual Checks
1. Group assignment and error checks together, then add a blank line before the next group.
2. Prefer early returns and avoid deep nesting by flattening control flow.
3. Prefer package boundaries by domain rather than a monolithic services package.
4. Use repository or interface boundaries for persistence and external integrations.
5. Centralize repeated string literals in `constants.go` per package.
6. Define sentinel errors in `errors.go` per package using `errors.Is/As`.
7. Use exported const groups as enums with a shared prefix.
8. Order file contents: package vars, types, constructors, methods, exported funcs, unexported funcs.
9. Keep comments meaningful and user-focused when added; avoid noise.

## Thermonuclear Adversarial Review

Use this review style whenever the user requests a repository review. Treat the implementation as guilty until its contracts, failure behavior, and operational assumptions are demonstrated by code or tests. Do not make code changes during the review unless explicitly requested.

### Required Passes

1. **Contracts and architecture:** Trace composition roots, package ownership, dependency direction, interfaces, public APIs, and serialization boundaries. Flag leaks between domains, ambiguous ownership, unnecessary coupling, and APIs that permit invalid states or misuse. Check for bloated parameter lists, unrelated parameter threading, speculative interfaces, and indirection that only forwards calls.
2. **Correctness and state:** Derive invariants and state machines. Check initialization, empty and duplicate inputs, ordering, retries, cursor advancement, idempotency, stale data, partial success, restart behavior, and clock boundaries.
3. **Failures and cancellation:** Inspect every I/O and goroutine boundary for timeouts, cancellation, error wrapping, classification, cleanup, retry storms, lost errors, secret leakage, and shutdown behavior.
4. **Concurrency and liveness:** Examine shared state, ownership, synchronization, races, deadlocks, starvation, boundedness, backpressure, goroutine lifetime, and behavior with concurrent callers and multiple instances.
5. **Persistence and SQL:** Review schema constraints, query semantics, transaction boundaries, migrations, generated-code boundaries, connection lifecycle, nullability, indexes, and consistency under failure. Verify that in-memory and durable implementations honor the same interface contract.
6. **External protocols:** Adversarially test status classes, malformed bodies and headers, rate limits and buckets, caching validators, pagination, authentication, webhook responses, upstream outages, and protocol-specific retry rules.
7. **Security and operations:** Check configuration validation, least privilege, unsafe defaults, log and metric contents, health/readiness, resource limits, deploy/restart behavior, and dependency or credential exposure.
8. **Efficiency and maintainability:** Look for unbounded memory, excess allocations or requests, accidental quadratic work, needless abstractions, misleading names, poor modularity, formatting, idiomatic Go violations, and missing documentation. Check that coupled assignments and immediate error checks have no intervening blank line, while distinct logical phases are separated by blank lines; reject both choppy whitespace and visually merged phases.
9. **Tests and tooling:** Test the adversarial matrix below, inspect test isolation and assertions, and run repository verification plus targeted race/static/tooling checks. Treat generated files and task commands as production code.

### Adversarial Matrix

Exercise or reason through nil, empty, malformed, oversized, duplicate, reordered, stale, and boundary inputs; context cancellation at every wait and I/O; partial dependency failure; all relevant 2xx, 3xx, 4xx, 5xx, invalid-body, and invalid-header responses; concurrent calls; clock skew; restart; rate-limit exhaustion; and multi-replica execution. Distinguish behavior proven by tests from behavior inferred only from inspection.

### Findings Standard

Report findings first, ordered by impact as Critical, High, Medium, then Low. Each finding must include an absolute file and line reference, the concrete failure mode, why it violates a contract or creates risk, and a minimal reproduction or proof when practical. Separate confirmed defects from assumptions and residual coverage gaps. Do not report mere preferences as defects, and do not bury actionable findings beneath a summary.

## Closeout

1. Re-check changed paths against the DOX chain
2. Update nearest owning docs and any affected parents or children
3. Refresh every affected Child DOX Index
4. Remove stale or contradictory text
5. Run existing verification when relevant
6. Report any docs intentionally left unchanged and why

## User Preferences

When the user requests a durable behavior change, record it here or in the relevant child AGENTS.md

## Service Module Rules

- The root Go module is `github.com/btnmasher/rex` and owns `cmd/` and
  `internal/`; `lib/jobruntime` remains a separate reusable module at
  `github.com/btnmasher/rex/jobruntime`.
- `internal/authnextdb` owns SQL issued against shared auth-next tables except
  token export, which is owned by `internal/tokenexport`.
- Structure enrichment is keyed by globally unique structure ID; never scope a
  tracked structure lookup by the corporation that polled the notification.
- `internal/universe` owns memory-cache-first, database-second EVE identity and
	location enrichment with public ESI fallback, including character,
	corporation, alliance, region, and structure-type names; alert rendering may
	treat missing enrichment as non-fatal.
- `internal/notifications` ignores ESI `is_read`; cursor and notification-ID
  deduplication state are authoritative for deciding whether to deliver alerts.
- `internal/token` owns the in-memory corporation access-token pool and rotation
  lifecycle. It must not query auth-next or persist token material.
- Per-corporation token rotation uses context-aware keyed serialization; canceled
	  waiters must release their registry references and must not retain lock entries.
	  Refresh fetches are coalesced and publish complete token snapshots atomically
	  without holding the rotation lock during network I/O.
- `internal/esi`, `internal/discord`, and `internal/tokenexport` own their
	 respective external API boundaries. Discord transport errors must remain
	 safe for logs and durable retry metadata while preserving unwrap behavior;
	 transient transport failures are normalized as retryable outcomes while
	 caller cancellation is not. Same-webhook requests may proceed concurrently
	 with bounded in-process cooldown state applied after provider-reported
	 backoff.
- `internal/logging` owns logger construction and terminal-oriented pretty
  rendering. Pretty output is opt-in and TTY-aware; non-interactive output
  remains structured JSON.
- `lib/jobruntime` isolates claimed-item workflow failures; every claimed item
  must be transitioned to retry or terminal failure before its batch completes.
  Panic cleanup derives a bounded recovery context from the caller context,
  retaining context lineage while allowing failure marking after cancellation.
- `cmd/rex` owns the application signal context and waits for token refresh and
  notification workers to stop before shared resources are closed. Its
  `--migrate` and `--discord-debug` modes are the single packaged entry points
  for maintenance and local alert-format testing. The service is deployed as
  a single instance; retry draining is not a distributed lease.
- `internal/poller` owns one-minute corporation scheduling, cursors,
  corporation-wide deduplication, pre-routing, and atomic delivery admission.
  Eligible
  notifications are processed in timestamp order and stop at the first
  persistence failure so the cursor cannot skip unresolved work; future
  upstream timestamps are clamped to current UTC before cursor storage.
- `internal/routing` owns canonical selector compilation, pre/post destination
  filtering, corporation filters, structure-type exclusions, and duplicate
  target rejection.
- `internal/enrichment` owns the provider-neutral enriched notification
  context consumed by destination adapters.
- `internal/delivery` owns the asynchronous queue worker, provider-neutral
  outcomes, retry policy, cancellation, and terminal delivery cleanup. It must
  not inspect provider payloads or perform alert filtering.
- `jobruntime/scheduler` owns wall-clock-aligned periodic boundaries. Scheduled
  callbacks are non-overlapping and skip boundaries missed during a running
  callback rather than catching up.
- `internal/notificationstate` owns the cursor-store contract; the SQLite
  implementation persists per-corporation character cursors, a global unique
  seen-notification ledger, bounded retry metadata/payloads, and 30-day alert
  history. Cared-for IDs are claimed atomically with their retry payload before
  delivery. Pending payload size validation is shared by all store
  implementations, and malformed durable retry rows are dropped while listing
  due work. Alert history records one terminal outcome per destination, including
  status, terminal error, raw notification, classified event, and formatted
  Discord JSON; do not persist webhook URLs or access tokens in this store.
  Transient delivery failures remain in the retry queue until successful,
  exhausted, or stale. Globally seen notification IDs are retained for one hour.
  The configured lookbehind is a
  hard maximum age for fetched notifications; stale rows are marked seen and
  advance their character cursor without delivery. New streams use the same
  window when their initial cursor is created. Delivery retries carry their
  first-queued timestamp and are dropped, marked complete, and cursor-advanced
  after thirty minutes. Delivery history writes are retried independently a
  bounded number of times after a successful send; a history write failure is
  logged without redelivering the alert. Expired retries record all affected
  destination IDs before queue deletion when the payload can be decoded.
- `internal/esi` keeps process-local ETag and rate-limit state bounded
  with stale-entry and capacity eviction. Public universe lookups do not use
  conditional ETags because the client does not retain response bodies for a
  304. Header-derived retry and moving-window delays are trusted up to a
  30-minute safety cap, keyed by the configured ESI client identity.
- SQLite and PostgreSQL job-store migrations use embedded Goose providers and
  the `goose_db_version` table. Automatic PostgreSQL job-store migrations run
  only when `JOB_STORE=postgres`; explicit migration commands select their
  backend by `--store` and require the matching database configuration.
  Auth-next tables remain externally owned and are never migrated by Rex.
  Production Compose persists SQLite state through the Docker-managed
  `rex-state` volume; a machine-local Compose override can replace it with the
  host `.data/` bind mount for development, with host permissions managed
  locally.
- `task ci:checks` is the canonical pre-merge gate: it runs verification,
  race tests, and rejects generated or formatted drift. Release tags publish
  the non-root multi-architecture Docker image to GHCR after those same gates
  pass.
- `internal/alerts` may run without a structure database; missing enrichment is
  non-fatal, and ownership-transfer alerts use the informational blue default.
  Full-power structure transitions use the success green default. Both
  `internal/alerts` and `internal/discord` must keep rendered
  embeds within Discord's published limits and escape upstream or hydrated
  display text before placing it in Markdown-capable fields.
- Bulk `StructuresReinforcementChanged` payloads may identify multiple
  structures without a system ID; parse their bounded structure list and use
  globally keyed structure enrichment to render deduplicated system and region
  coverage grouped by region, solar system, and structure type.
- A `StructuresReinforcementChanged` weekday value of `255` means the weekday
  was unchanged in the observed EVE notification format; render it as
  `Unchanged`, not as a calendar day.
- Alert filtering is a service-level parent-group/leaf allowlist with
  destination-scoped corporation allowlists/blocklists, leaf exclusions, and
  structure-type exclusions, while destination mappings are corporation-scoped
  and many-to-many. Corporation exclusions take precedence over inclusions;
  preserve both boundaries when extending notification routing.
- Accepted destination IDs are persisted with pending rows, so a retry after a
  partial delivery does not resend destinations already accepted.
- Identical Discord webhook URLs are flattened after post-routing filters are
  applied; pre-routing retains every candidate because shared URLs can have
  different destination filters. One notification must produce at most one
  delivery per URL for the surviving targets.
- Starbase notification routing uses the canonical `starbase` group with
  `starbase.under_attack` and `starbase.resource_alert` leaves. Canonical dot
  paths are the only supported alert selectors.
- Customs-office notification routing uses the canonical `customs_offices`
  group with `customs_offices.attacked` and `customs_offices.reinforced` leaves.
- `CorpStructLostMsg` is intentionally unsupported; do not reintroduce it as a
  structure destruction or combat alert without verified modern payload semantics.
- Keep auth-next bearer credentials and access tokens out of logs, errors,
  persistence, and payload templates.
- The access-token export URL and bearer credential are required configuration;
  the EVE client ID is used for ESI rate-limit identity. No EVE client secret is
  required by this service.
- The `access-token-refresh` job lists eligible corporations every fifteen
  minutes and fetches each corporation independently with bounded concurrency.
  `AUTH_NEXT_TOKEN_EXPORT_COUNT` controls how many identities auth-next returns
  per corporation, from 1 through 64, defaulting to 60. Up to sixty-four token
  identities are retained per corporation, and normal polling does not reuse
  an identity within ten minutes. The smoothing threshold is derived from the
  configured poll interval, so a 30-second cadence requires twenty identities
  before every cycle can proceed without stretching.
- Alert links show hydrated names without parenthesized IDs by default;
  destination `presentation.showEntityIDs` enables IDs while unresolved IDs
  remain link fallbacks. Destination configuration is loaded from
  `ALERT_DESTINATIONS_CONFIG` as strict versioned JSON or YAML.
- Ownership-transfer alerts render previous and new corporation ownership
  separately, using hydrated names with EVEWho fallback links.
- Structure alerts use the ESI payload's owner corporation when present. When a
  new structure has no owner metadata and is not yet tracked in auth-next, use
  the polling character's corporation context instead.
- Production contexts must derive from the main application context chain; do
  not use `context.Background()` for runtime I/O, database initialization,
  migrations, or shutdown-sensitive work. `context.Background()` is reserved
  for true process roots and tests.
