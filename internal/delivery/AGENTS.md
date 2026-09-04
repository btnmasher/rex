# Delivery Boundary

## Purpose

Own asynchronous notification admission, delivery, retry, and terminal cleanup.

## Ownership

- `Worker` owns durable queue draining and bounded in-process concurrency.
- Destination adapters own presentation and provider transport.
- `enrichment.Enricher` is the sole enrichment dependency; delivery does not own
  universe lookup or destination presentation.
- Routing and enrichment are dependencies; this package does not inspect EVE payload semantics.

## Local Contracts

- Admission commits cursor state and pending delivery state together through `notificationstate.Store`.
- Retry state contains only provider-neutral target IDs and serialized notification data.
- Accepted targets are not retried when another target fails.
- Canceled work does not create new retry attempts.

## Work Guidance

- Keep queue work bounded and panic-isolated.
- Derive all runtime I/O from the caller context.
- Normalize adapter outcomes before changing durable retry state.

## Verification

- Run `task test:service` and `task test:race` for delivery changes.

## Child DOX Index
