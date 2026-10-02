# Design notes

Deviations, ambiguities and decisions taken while implementing the roadmap.
Newest entries last. Each entry references the milestone/task it came from.

## M0.1 — Repo init

- **Tooling pinned as Go `tool` directives.** `.golangci.yml`-driven linting
  (`golangci-lint`), `sqlc`, `goose` and `air` are declared in `go.mod` via
  `go get -tool`, so `make lint` / `make sqlc` / `make dev` work from a fresh
  clone with no extra installs. `go.sum` is large because of these trees; the
  shipped binary is unaffected (build is `CGO_ENABLED=0 -trimpath`).
- **`CGO_ENABLED=0` only on build targets.** `make test` runs `go test ./... -race`,
  and the race detector needs cgo on linux/amd64, so the Makefile does not
  export `CGO_ENABLED=0` globally (architecture rule 7 concerns the build).
- **`go 1.26.8` in go.mod.** The toolchain installed on this host is go1.26.8;
  AGENTS.md asks for "Go 1.23+", so this satisfies it.
- **`make migrate-new` is a shell script** (`scripts/migrate-new.sh`) rather than
  `goose create`, so migrations land in `internal/store/migrations` with the
  `NNNN_snake_case.sql` naming from `02-data-model.md` regardless of cwd.
- **`make openapi` is wired in M0.6**, together with `api/openapi.yaml`; the
  target exists (and no-ops) so `make help` and CI stay complete.
## M0.2 — Backend skeleton

- **`internal/logging`** (not in the AGENTS.md layout list) builds the JSON
  `slog` logger; `main` only wires it. `logging.Attrs` converts key/value
  pairs into attributes for the access log's conditional fields.
- **Request IDs are ULIDs** (`internal/id`), matching the `"01J…"` example in
  `01-api.md`. An inbound `X-Request-Id` is reused when it is at most 64
  characters, otherwise a new ULID is minted and echoed in the header and in
  the error envelope.
- **`/api/v1/system/info` has a temporary seam.** `service/system.New` takes
  optional `UserCounter`/`HypervisorChecker` seams so the route exists in M0.2
  while the store and hypervisor land in M0.3/M0.4; `main` passes `nil`, hence
  `needs_setup:false` and `hypervisor.ok:false` until then. M0.3 wires the
  store, M0.4 the hypervisor.
- **`quota_exceeded` maps to HTTP 429.** `00-architecture.md` §7 maps the other
  sentinels but not the quota one; 429 follows the usual "too many / not enough
  capacity" convention.
- **`method_not_allowed`** was added to the error-code list in `01-api.md` §
  Conventions (the doc lists no code for HTTP 405). Unknown routes use the
  existing `not_found` code.
- **gin mode follows `KUMO_LOG_LEVEL`** (`debug` → debug, otherwise release) so
  gin never prints its own routes in production logs.
- **`sloglint.no-raw-keys` is disabled** in `.golangci.yml`: in sloglint v0.11.1
  it flags every *literal* key, including the keys inside `slog.String`/
  `slog.Attr`, so keeping it on would ban the idiomatic style. `key-naming-case:
  snake`, `no-mixed-args`, `static-msg` and `context: all` stay on.
- **Timeout policy for `http.Server`:** `ReadHeaderTimeout` 10 s, `IdleTimeout`
  120 s, graceful `Shutdown` 15 s; no write timeout, because SSE (`/events`,
  M5) and the console WebSocket will hold connections open.

## M0.3 — Store

- **`internal/store/sqlc` is generated** from `internal/store/migrations`
  (schema) and `internal/store/queries` (queries) by `sqlc.yaml`; `make sqlc`
  regenerates it and the generated files are committed. Only `CountUsers`
  exists today — each milestone adds its queries next to its service.
- **sqlc query type `:scalar` does not exist** in sqlc v1.30 (`docs` here use
  plain SQL, so this is only a note for query authors): `COUNT(*)` uses `:one`,
  which sqlc maps to `int64`.
- **Pragmas are set through the DSN** (`_pragma=journal_mode(WAL)`, …) instead of
  per-connection statements, so every pooled connection gets them.
  `SetMaxOpenConns(1)` keeps a single writer, as `02-data-model.md` suggests.
- **goose's package-level configuration is set once** via `sync.OnceValue`:
  `SetBaseFS`/`SetDialect`/`SetLogger` write globals, and `make test` runs
  `-race` with parallel tests that migrate several databases at once.
- **`store.Exec` / `tx.Exec`** are the only place a parameterized statement can
  be issued outside `internal/store/queries`; they exist for tests and future
  maintenance tasks, and services use the sqlc queries.
- **`store.Open` creates the data directory** (0700) when it is missing, so a
  fresh install does not need `mkdir -p /var/lib/kumo`.
