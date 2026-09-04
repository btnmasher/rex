# Jobruntime Rules

## Purpose

This module provides domain-neutral job execution and interchangeable
ephemeral or SQLC-backed PostgreSQL stores.

## Ownership

- jobstore defines the stable lifecycle interface and data types.
- scheduler defines wall-clock-aligned, non-overlapping periodic execution for
  callers that need recurring job orchestration.
- jobstore/memory owns process-local state for tests and single-process jobs.
- jobstore/sqlite owns the SQLC-backed single-process durable implementation.
- jobstore/postgres owns SQLC queries and only jobruntime-owned job tables.
- jobs depends on the interface, never on pgx or application domains.

## Local Contracts

- Preserve behavior across both store implementations where the interface
  promises it.
- Keep SQL in SQLC query declarations and generated clients; do not add ad hoc
  SQL in runner code.
- Domain-owned producers supply bounded work batches; the runner owns queue
  insertion and checkpoint persistence. Producers are optional for queue-only
  jobs.
- `InsertItems` must be safe to repeat for an existing item with the same
  payload, and must reject a conflicting payload rather than overwrite work.
- Checkpoints are opaque to the runtime and may be absent. Typed checkpoint
  formats belong behind producer-owned adapters.
- Durable runs and inflight items use bounded stale-state recovery, and empty
  claims must distinguish delayed retries or inflight work from a drained run.
- Reclaimed item mutations must be fenced by the claimed attempt count.
- Recover panics at item and run boundaries: item workflow panics must enter the
  configured bounded retry path, while lifecycle panics must return an error
  and best-effort mark the run failed. Log panic type and stack context without
  persisting the panic value.
- Public runner entry points reject nil contexts immediately; runtime recovery
  derives bounded cleanup contexts from the caller context.
- Store constructors apply only the embedded jobstore migrations for
  jobruntime-owned durable state; SQLite and PostgreSQL never migrate
  application-owned tables.
- Durable store constructors accept a caller-owned context and must use it for
  connection initialization and Goose migrations; callers must derive it from
  the application or command context.

## Verification

From this module directory, run `go test ./...`, `go vet ./...`, and
`go test -race ./...`.

## Child DOX Index

- `scheduler/AGENTS.md` defines the wall-clock scheduling contract.
