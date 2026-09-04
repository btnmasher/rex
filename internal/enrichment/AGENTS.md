# Enrichment Boundary

## Purpose

Build one provider-neutral notification context from classified EVE data and best-effort local or ESI lookups.

## Ownership

- `Service` owns structure, location, identity, ownership, attacker, and type enrichment.
- Destination adapters consume `Context` and do not perform enrichment.
- `Enricher` is the package-owned interface consumed by delivery workers and
  destination adapters.
- Missing database or ESI data is non-fatal; unresolved values remain available to presentation fallbacks.

## Local Contracts

- Structure lookup is global by structure ID, never scoped by polling corporation.
- Payload-provided owner and type data takes precedence over polling context and database enrichment.
- The input envelope is not mutated; returned context owns copied raw payload data.

## Work Guidance

- Keep external lookup failures isolated to the affected field or entity.
- Derive all lookups from the caller context.
- Add bounded batch lookups rather than one-off request plumbing when a notification contains multiple structures.

## Verification

- Run `task test:service` and `task test:race` for enrichment changes.

## Child DOX Index
