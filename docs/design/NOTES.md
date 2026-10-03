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

## M0.6 — Embed + OpenAPI

- **The console is copied into the package before the Go build.** `make
  web-build` runs `npm run build` and then replaces `internal/webui/dist` with
  `web/dist` (keeping `.gitkeep`), because `go:embed` cannot reach outside the
  package directory. The directory is git-ignored except for `.gitkeep`, so no
  build output is ever committed.
- **`//go:embed all:dist` includes dotfiles on purpose**: with only `.gitkeep`
  present the Go build still succeeds, so a fresh clone compiles and `kumod`
  starts without a console. `webui.Handler.Built()` reports that case and `main`
  logs a warning; the API keeps working.
- **The SPA fallback only answers paths that look like routes.** A missing file
  with an extension (`/assets/index-stale.js`) is a 404, because Vite writes
  hashed names and answering a stale asset with HTML only produces a confusing
  parse error in the browser. Everything else falls back to `index.html` so a
  deep link such as `/instances/i-…` survives a reload. `..` in a path is
  rejected before the embedded FS is touched.
- **The console is injected as an `http.Handler` (`api.Options.Console`) rather
  than imported from `internal/webui` by the router.** `webui` is wired in
  `main`, which keeps `internal/api` testable (the router tests use a stub
  handler) and keeps the layering rule that handlers never depend on
  infrastructure.
- **API paths keep the JSON envelope even with the console mounted.** The
  console is only mounted through `NoRoute` and skips `/api/...`, so an API
  client that mistypes a URL gets the `not_found` envelope with a `request_id`,
  not HTML.
- **`assets/*` is served `immutable` for a year, everything else `no-cache`.**
  Vite content-hashes the bundle names, so the shell is the only file that must
  be revalidated after a deploy.
- **`api/openapi.yaml` covers the whole v1 contract from `01-api.md`** (24 paths,
  32 operations) even though only `/system/info` is implemented: the spec is the
  contract the console is generated from, and M1–M5 fill in the handlers. State
  enums come from the `CHECK` constraints in `02-data-model.md`, so
  `InstanceState`/`ImageState`/`TaskState` cannot drift from the database.
- **`/images/upload` (optional, M3b) and `/events` (SSE, "later") are
  deliberately absent from the spec**, so the console does not generate calls to
  endpoints that do not exist. `internal/api/spec_test.go` encodes that omission,
  and also fails when the spec and `01-api.md` disagree in either direction or
  when a `$ref` does not resolve — the AC of this milestone cannot be checked by
  hand once the spec grows.
- **`openapi-typescript` is pinned to 7.13.0 with a `typescript` override.**
  Its peer range is `^5.x` while the console is on TypeScript 6; the generator
  does not use TypeScript at runtime (only for its optional output validation),
  so `package.json` maps its peer to the workspace version instead of pinning an
  older compiler. `npm run gen:api` regenerates `web/src/types/api.ts`, which is
  committed; feature code imports from there and never hand-writes a DTO.

## M1.1 — Backend auth

- **Access tokens are HS256, not EdDSA.** `01-api.md` allowed either. A control
  plane signs and verifies on the same host, so there is no public key to
  distribute and no RSA/Ed25519 keygen to manage; switching to EdDSA later only
  changes `internal/auth.Signer` and the `alg` in the token header. The key is
  32 random bytes in `KUMO_JWT_SECRET_FILE` (default `$DATA_DIR/jwt.key`),
  generated on first run with mode `0600`, and a file readable by group or other
  is rejected at startup rather than used.
- **The role claim lives in the token payload, not the header.** `role` is a
  custom claim next to `sub`/`iat`/`exp`/`jti`; putting it in the JOSE header
  would be signed as well but is semantically the wrong place for it.
- **`alg: none` and RS256 tokens are rejected structurally**, not by checking a
  string: `jwt.WithValidMethods` plus a key function that only returns the key
  for an HMAC method, so a forged token fails as `ErrInvalidToken` — the same
  answer as an expired one.
- **Two packages, one per layer.** `internal/auth` holds the primitives
  (`password.go`, `token.go`, `secret.go`) with no database or gin dependency;
  `internal/service/auth` owns users, refresh tokens and the login rate limiter,
  and `internal/api` only translates HTTP to service calls. `internal/service`
  grew a `ValidationError`/`FieldError` pair so a service can report per-field
  problems, and `internal/api.describeError` maps it to the same 400 envelope
  the api layer used to build by hand.
- **Refresh tokens are 256-bit random values stored as SHA-256 hashes.**
  The plain token only ever appears in the `HttpOnly; Secure; SameSite=Strict`
  cookie, the database keeps `token_hash`, and every rotation carries the
  `family_id` of its predecessor. Presenting an already revoked token revokes
  the whole family and answers 401: a replay means the token leaked, so the
  attacker and the victim both have to log in again.
- **Login failures are uniform in body *and* in timing.** An unknown username
  runs two Argon2id hashes before answering 401, so response time does not leak
  which accounts exist, and the error message never repeats the username.
- **The rate limiter is an in-memory fixed-window table (5/min per IP+username,
  in `internal/service/auth/limiter.go`).** A control plane is a single
  process; losing the counters on restart costs an attacker five attempts, and
  no extra dependency is needed for a counter. The window starts at the first
  failure and is cleared by a successful login. Keyed on
  `strings.ToLower(remoteAddr) + username` lowercased, because usernames are
  stored case-insensitively.
- **The refresh cookie is scoped to `/api/v1/auth`.** Only the auth endpoints
  need it, so a narrower path keeps it off every other request; `Secure` follows
  `KUMO_COOKIE_SECURE` (true by default, false only for plain-HTTP development).
- **Argon2id parameters (64 MiB, t=3, p=2, 32-byte key, 16-byte salt) are stored
  inside the hash** and read back on verification, so raising the defaults later
  does not lock out existing users. `p=2` rather than `p=4` keeps the test suite
  usable on small machines.
- **The logout cookie uses `Path=/api/v1/auth`** and is expired with `MaxAge=-1`,
  which is the only way to delete a cookie from the browser: it has to match the
  path and name it was set with.

## M1.2 — CLI helpers

- **The command line lives in `internal/cli`, and `cmd/kumod` is only wiring.**
  `cli.Run(ctx, cli.Env{Args, Stdin, Stdout, Stderr})` returns an exit code, so
  every command (`serve`, `admin create`, `admin reset-password`, `doctor`,
  `version`) is testable without spawning a process or touching the real
  environment. `serve` moved out of `main.go` for the same reason: AGENTS.md asks
  for wiring-only `main()`, and the server path is now covered by the same
  dispatch the tests use.
- **No password flag, ever.** `kumod admin create --password` would put the secret
  in `argv`, which every process on the host can read through `/proc`. The
  password comes from the first line of stdin (`--password-stdin`) or from
  `KUMO_ADMIN_PASSWORD`, and the policy is checked *before* the database is
  opened so a typo is reported without touching the file. v1 has no interactive
  prompt: that would need terminal handling (and a `x/term`-style dependency)
  for no gain, since an operator can pipe a file.
- **`admin create` is for the second admin, not the first one.** The first-run
  path stays `POST /auth/setup`, which only works while the instance has no
  users; `CreateAdmin` is what an operator uses on a running instance. A taken
  username is `already_exists`, matched case-insensitively to match the
  `COLLATE NOCASE` unique index rather than by parsing the driver error type.
- **`admin reset-password` revokes every refresh token of that user, in the same
  transaction as the password update.** A reset means the old password is
  compromised; leaving the sessions it opened alive would make the command
  useless for exactly the incident it exists for.
- **`doctor` is honest about what it does not check yet.** It verifies the
  configuration, the data directory (including a write probe), the free disk
  space, the database (open + migrate + user count), the signing key (creating
  it when missing, so a fresh host is ready to start) and the hypervisor backend.
  The libvirt socket, `/dev/kvm`, pools and firewall checks of
  `04-libvirt-integration.md` §8 are printed as `skip` with the milestone that
  implements them, instead of being faked. `doctor` fails when the configured
  backend is libvirt and M3.1 has not landed, because the server would refuse to
  start for the same reason.
- **Free disk space uses `syscall.Statfs` behind a `//go:build linux` tag.** The
  doctor reports "not measurable" elsewhere rather than pulling in
  `golang.org/x/sys`, which AGENTS.md would want approved first.
- **A CLI invocation of `kumod admin …` opens the database with `store.Open`, so
  migrations still apply.** The commands must work against a database a previous
  version wrote, and a migration is the only thing that can guarantee the schema
  is what the queries expect.

## M1.3 — Console login

- **The session lives in `auth` slice state and nowhere else.** `{user,
  accessToken, status}` with `status: 'unknown' | 'authenticated' | 'anonymous'`
  (03-frontend.md §4). The access token is never persisted: `localStorage` would
  hand it to any XSS, and the refresh cookie is `HttpOnly` anyway. `status`
  starts `unknown` rather than `anonymous` because the first render of a reload
  happens before the refresh cookie has been spent, and redirecting there would
  throw a signed-in operator to the login page on every reload.
- **`baseQuery` is a three-step ladder: attach, refresh once, replay.** A console
  page may stay open for days while an access token lives 15 minutes, so the
  expiry is the normal case. The endpoints whose own 401 *is* the answer
  (`login`, `setup`, `refresh`, `logout`) are excluded, or a wrong password
  would cost a refresh round trip and dispatch a sign-out the form must ignore.
  A second 401 is returned as is rather than retried forever.
- **The refresh is single-flight for a reason beyond efficiency.** Refresh tokens
  rotate, and the backend treats a replayed one as a stolen session and revokes
  the whole family. A page load fires several requests at once; without the
  shared promise in `app/authSession.ts`, each 401 would rotate the cookie
  independently and lock the operator out for being busy. `api.test.ts` asserts
  two concurrent 401s cause exactly one rotation.
- **The refresh bypasses RTK Query on purpose** (`fetch` in `authSession.ts`, not
  an endpoint): it is the thing every endpoint's 401 handling depends on, so it
  cannot go through a `baseQuery`, and its answer is a token, not cacheable
  state.
- **`fetch` needs absolute URLs, so the base URL is `${location.origin}/api/v1`.**
  Leaving it relative works in the browser but not in jsdom, which keeps Node's
  `fetch`. Same-origin either way, so the `SameSite=Strict` cookie is unaffected.
- **`/setup` signs the operator in afterwards.** `/auth/setup` creates the first
  administrator and returns no token, so the page logs in with the credentials it
  just submitted: making them type the same password into a second form would be
  a strange reward for following the setup instructions.
- **The pages supply their own `<form>`.** Cloudscape's `Form` renders no form
  element and takes no `onSubmit`; `Button formAction="submit"` is just
  `type="submit"`. Without a form of our own, Enter in a field would reload the
  page. A test presses Enter for exactly that reason.
- **Mutation triggers are called, not dispatched.** `dispatch(login({...}))` is
  the documented shape for *queries*; dispatching a mutation trigger passes a
  promise into the store and redux throws "Actions must be plain objects".
- **`next` is restricted to in-app paths.** `?next=` comes from the URL, so an
  absolute URL there would make the login page an open redirect.
- **Sign-out clears the query cache.** `logout`'s `onQueryStarted` dispatches
  `loggedOut()` and `resetApiState()` even when the request fails: the session is
  over locally whether or not the server acknowledged it, and the next operator
  must not inherit the previous one's cached lists.
- **MSW keeps the cookie jar**, so `Set-Cookie` on login/refresh is stored and
  sent back the way the browser does it. That is what makes the "a reload keeps
  the session" test real: the test signs in on one store, throws it away, and the
  new store recovers from the cookie alone.
- **The top navigation is measured, not laid out.** Cloudscape decides what fits
  in it with a `ResizeObserver`, so jsdom (where ours is a no-op stub) keeps the
  offscreen copy in the DOM and cannot show the signed-in name; the tests drive
  the menu through that copy and say why.
