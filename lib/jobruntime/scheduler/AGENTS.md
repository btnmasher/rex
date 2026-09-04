# Scheduler Rules

## Purpose

Provide the module's wall-clock-aligned, non-overlapping periodic scheduler.

## Ownership

- This package owns boundary calculation, timer cancellation, and missed-tick
  behavior.
- Callers own the work invoked at each scheduled boundary and its logging or
  error handling.

## Local Contracts

- Scheduling uses the next wall-clock boundary for the configured interval.
- A callback runs at most once at a time; boundaries missed during a callback
  are skipped rather than replayed.
- The caller context controls waiting and shutdown.

## Verification

From this module directory, run `go test ./...`, `go vet ./...`, and
`go test -race ./...`.

## Child DOX Index

No child instruction files.
