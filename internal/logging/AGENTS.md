# Logging Package Rules

## Purpose

Own Rex logger construction and the optional local-development presentation of
structured `slog` records.

## Ownership

This package owns the `LOG_PRETTY` behavior and the custom `slog.Handler`. It
does not own application log messages, configuration parsing, or external
service concerns.

## Local Contracts

- Pretty output is used only when explicitly enabled and stderr is an
  interactive terminal.
- Non-interactive output must remain newline-delimited JSON for operational
  tooling.
- Payload logging is opt-in through `LOG_PAYLOADS` and must remain bounded.
- The pretty implementation must preserve `slog` levels, attributes, groups,
  derived handlers, and concurrent writes.
- Never log credentials, access tokens, webhook URLs, or other secrets.

## Work Guidance

- Keep presentation concerns separate from logger composition and config
  parsing.
- Prefer standard-library `log/slog` contracts over package-specific logging
  APIs.
- Keep tests focused on handler semantics and output selection.

## Verification

- Run `task verify` after changes.
- Run `task test:race` when changing handler state or synchronization.

## Child DOX Index

No child instruction files.
