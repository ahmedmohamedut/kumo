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

## M0.4 — Hypervisor

- **`internal/hypervisor` is the whole contract**: interface, DTOs
  (`InfraConfig`, `ImageInfo`, `RootDiskSpec`, `CloudInit`, `DomainSpec`,
  `DomainInfo`, `Capacity`, `DomainEvent`) and the sentinel errors
  `ErrNotFound`/`ErrAlreadyExists`/`ErrInvalidState`/`ErrNotImplemented`. Every
  operation is also addressable by a `hypervisor.Op`, which the fake uses for
  failure injection.
- **The fake is the reference implementation** of the state machine: a domain is
  created by `DefineAndStart` (needs its root disk volume, so a failed launch
  is visible), `Start` only from `shutoff`, `Shutdown` only from `running`,
  `Destroy` is idempotent like libvirt, `Reboot`/`Reset` need `running`.
  Invalid transitions return `ErrInvalidState`, unknown ids `ErrNotFound` —
  services decide which of those are tolerable during a partial teardown.
- **The fake is a synchronous in-memory implementation**; `Latency` is applied
  in one place (`Fake.begin`) together with the context and failure checks, so
  every method honours `ctx` the same way and tests can drive timeouts.
- **`EnsureInfra` only records its arguments in the fake** (it must not touch
  the host); the real backend creates the pool directories and the network in
  M3.2. The fake still needs `EnsureInfra` to know the pool names, which is
  what routes volume keys.
- **The backend is chosen in `cmd/kumod`**: a small `newHypervisor` switch keeps
  `internal/hypervisor` free of a dependency on its own implementations (the
  fake imports the parent package for the DTOs). `KUMO_HYPERVISOR=libvirt`
  currently fails fast with `hypervisor.ErrNotImplemented` — the real backend
  lands in M3.1, and a control plane that boots without one would be lying about
  what it can do.
- **`/readyz` now probes `db` and `hypervisor`**, and `/api/v1/system/info`
  reports the backend from `system.HypervisorChecker`, so the seam introduced in
  M0.2 is exercised end to end.

## M0.5 — Frontend skeleton

- **The Vite/React scaffold was replaced, not extended**: the starter
  `App.tsx`/`App.css`/logo assets are gone; `src/main.tsx` is now the only
  entrypoint and mounts Redux → PageMeta → Router → Cloudscape
  `AppRouter` with `@cloudscape-design/global-styles`.
- **Chosen layout**: `ConsoleLayout` (`src/components/layout/ConsoleLayout.tsx`)
  with a fixed `TopNavigation` (brand, service menu, account menu, theme
  switch), side `Navigation` with the four sections from `03-frontend.md`
  (`Overview`, `Compute`, `Images`, `Security`), `Breadcrumbs` fed by a
  `PageMetaContext`, and `Flashbar` fed by `features/notifications`. Pages are
  placeholder `ContentLayout` shells until their milestone lands.
- **`PageMetaContext` lives in its own module and stores only primitives**
  (array of `PageMetaItem`), with the provider keyed by the memoised array.
  An earlier version stored a render callback in state, which re-rendered the
  shell on every navigation; the context value is now compared by key so
  breadcrumb updates do not cascade.
- **Navigation data is derived, not duplicated**: `navigation.ts` exports the
  item list once, builds the Cloudscape shape from it, and computes
  `activeHref` by prefix match (`/instances/i-1` highlights `Instances`).
  A test asserts every navigable route has an entry, so a new route cannot be
  added without navigation.
- **Theme preference is persisted by Cloudscape's `TopNavigation`**
  (`visualRef` + `onChange` → `applyMode`), which is why there is no separate
  theme slice or `localStorage` code.
- **`useAppDispatch`/`useAppSelector` are typed hooks in `app/store.ts`** and
  re-exported from the feature files; RTK Query keeps server state, so the
  only global slice in M0.5 is `notifications` (flashbar items). `app/api.ts`
  uses `credentials: 'include'` so the refresh cookie rides along once auth
  exists in M1.
- **Vite proxies `/api` to `http://127.0.0.1:8080`** (no rewrite — the API is
  already namespaced) so `make dev` gives a same-origin console. Verified by
  running `kumod` with `KUMO_HYPERVISOR=fake` on `:8080` and curling
  `http://localhost:5199/api/v1/system/info` through the dev server.
- **Testing**: Vitest + Testing Library + jsdom, with `ResizeObserver` and
  `matchMedia` stubs in `src/test/setup.ts` (Cloudscape needs both). 17 tests
  cover layout regions/navigation/theme toggle, notification reducer
  behaviour, and `activeHref`. MSW is intentionally not added yet — no
  component fetches in M0.5, and M0.6/M1.2 add it with the first real API
  slice. `vitest` transforms are slow in this environment (first run ~1 min),
  so `make test` can look hung; it is not.
- **`oxlint` config only enables the React hooks rules**; it caught
  `only-export-components` on `pageMeta.tsx`, which is allowed via
  `allowExportNames`. Oxlint is used instead of ESLint per the M0 plan.
- **Prettier is checked in CI** (`format:check` wired into `make lint`,
  `format` writes). `make fmt` still runs `golangci-lint fmt` for Go.
