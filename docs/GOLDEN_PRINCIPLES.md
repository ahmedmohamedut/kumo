# Golden Principles — kumo

These invariants are non-negotiable. Violations produce
`GOLDEN_PRINCIPLE_BREACH` signals and pause the cycle for human review.
The breach RECORD is closed only by a human; a breach whose findings name a
file may first get one bounded, audited corrective pass that must re-gate
(platform GP-003 / ADR-075).

Stylistic rules and architectural conventions (layering, no-cgo, sqlc, etc.)
live in `HARNESS.json` under `constraints.rules` and produce
`CONSTRAINT_VIOLATION` signals that the platform can auto-retry. The
principles below are the ones that get a human in the loop. They restate
the hard rules in `AGENTS.md`.

## GP-001 — Data access goes through the store layer

All database access goes through `internal/store` (sqlc-generated queries, goose
migrations). Handlers and services never build or run SQL themselves.

## GP-002 — State changes are traceable

Every state-changing operation on a resource is recorded as a task or an audit
event, so what happened to an instance, image or key pair can be reconstructed.

## GP-003 — Input validation

Validate and bound all input at the API boundary (names, sizes, URLs) before it
reaches a service. Image import URLs allow only `http(s)` and block link-local
and metadata ranges (SSRF).

## GP-004 — No sensitive data in logs

Never log passwords, tokens, private keys or `user_data`. A generated private
key is returned exactly once, in the create response, and is never stored.

## GP-005 — Authentication on the API

All `/api/v1/*` routes except `/auth/login`, `/auth/refresh` and `/healthz`
require a valid JWT. Refresh tokens are stored hashed.

## GP-006 — Errors are wrapped and mapped at the edge

Errors are wrapped with `%w`; services return sentinel domain errors
(`ErrNotFound`, `ErrConflict`, `ErrInvalid`, `ErrQuota`) and only `internal/api`
maps them to HTTP status, using the error envelope in `docs/design/01-api.md`.

## GP-007 — Resource IDs follow the documented formats

External identifiers use the prefixed formats from `internal/id` (`i-`, `img-`,
`key-`, `task-`) and UUIDv7 for users; internal auto-increment keys are never
exposed in API responses or URLs.
