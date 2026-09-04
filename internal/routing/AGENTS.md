# Routing Boundary

## Purpose

Compile canonical alert selectors and evaluate destination routing in two phases.

## Ownership

- Pre-routing applies alert leaf/group selectors and corporation filters.
- Post-routing applies enrichment-dependent structure-type exclusions.
- Target ID deduplication and target identity mapping are owned at this boundary.

## Local Contracts

- Exclusions take precedence over inclusions.
- Canonical dot-path selectors are expanded to leaves at configuration time.
- Duplicate opaque target IDs are rejected during policy compilation.

## Work Guidance

- Keep policy evaluation deterministic and free of external I/O.
- Return target IDs only; adapters own provider-specific presentation and transport.

## Verification

- Run `task test:service` and `task test:race` for routing changes.

## Child DOX Index
