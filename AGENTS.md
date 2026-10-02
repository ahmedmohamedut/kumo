# AGENTS.md

Guidance for AI coding agents (opencode and others) working on **Kumo**, an AWS-style private cloud on KVM/libvirt. Humans should follow it too.

## Read first

Before writing code, read the relevant files in `docs/design/`:

| File | Read when |
|---|---|
| `00-architecture.md` | Always (once per session) |
| `01-api.md` | Touching any HTTP endpoint or API client |
| `02-data-model.md` | Touching the DB, migrations, or domain models |
| `03-frontend.md` | Touching `web/` |
| `04-libvirt-integration.md` | Touching `internal/hypervisor` or instance/image lifecycle |
| `05-roadmap.md` | Picking the next task. Work **one milestone task at a time**, in order |

If code and design docs disagree, **stop and flag it**. Do not silently diverge. If a design change is justified, update the doc in the same PR.

## Project summary

- Monorepo: Go backend in repo root (`cmd/`, `internal/`), React console in `web/`.
- Backend: Go, Gin, `github.com/digitalocean/go-libvirt`, SQLite (`modernc.org/sqlite`, pure Go, no CGO), JWT.
- Frontend: React + Redux Toolkit (RTK Query) + Vite + TypeScript + Cloudscape Design System.
- Production artifact: a single binary `bin/kumod` with the built frontend embedded via `go:embed`.
- Scope of the current phase: **Instances, Images, Key Pairs, Auth**. Do not implement S3, databases, Redis, queues, secrets or Kubernetes yet.

## Commands

```bash
make deps          # go mod download + npm ci in web/
make dev           # backend (air) on :8080 + vite on :5173
make build         # web build -> embed -> go build -> bin/kumod
make test          # go test ./... -race  +  npm test
make lint          # golangci-lint run  +  oxlint + tsc --noEmit
make fmt           # gofmt/goimports + prettier
make migrate-new name=add_foo   # new goose migration
make sqlc          # regenerate sqlc code after editing SQL
make openapi       # validate api/openapi.yaml
```

Run `make lint test` before declaring any task done. If a command doesn't exist yet, create it in the Makefile as part of M0.

Dev without KVM: `KUMO_HYPERVISOR=fake` uses the in-memory fake. Never require real libvirt in unit tests.

## Repository layout

```
cmd/kumod/                  main(): wiring only, no logic
internal/
  config/                   env/flag/file config (KUMO_* env vars)
  api/                      Gin router, middleware, handlers, DTOs
    middleware/             auth, request-id, logging, recover, cors
  auth/                     password hashing, JWT issue/verify, refresh tokens
  service/                  business logic (instances, images, keypairs, users, tasks)
  hypervisor/               Hypervisor interface + libvirt impl + fake impl
  store/                    sqlc queries, migrations (embed), transactions
    migrations/
    queries/
  worker/                   async task runner + reconciler loop
  id/                       resource ID generation (i-, img-, key-)
  webui/                    go:embed of web/dist
api/openapi.yaml            API source of truth
web/
  src/
    app/                    store.ts, router, providers
    components/             shared UI (layout, status indicators, tables)
    features/<feature>/     api.ts (RTK Query), slice.ts, pages/, components/
    lib/                    utils, formatters, error handling
    types/                  API types (generated from openapi.yaml)
docs/design/                design docs
```

## Architecture rules (hard)

1. **Layering:** `api` → `service` → (`store`, `hypervisor`). Handlers never call libvirt or SQL directly. Services never import `gin`.
2. **Hypervisor is an interface** (`internal/hypervisor`). All libvirt access goes through it. The libvirt impl uses `go-libvirt` only, with no `virsh` shelling out and no cgo.
3. **libvirt is the source of truth for runtime state; SQLite is the source of truth for metadata.** A reconciler syncs them (see `00-architecture.md`).
4. **Long operations are async:** the handler creates a `task`, returns `202` with the resource in a transitional state, and a worker does the work. Never block an HTTP request on image download, volume creation, or VM boot.
5. **Idempotent, rollback-safe workflows:** every multi-step operation (e.g. launch instance) must clean up partial resources on failure.
6. **Single binary, zero external services.** Do not add Redis, Postgres, message brokers, or Docker as runtime dependencies of Kumo itself.
7. **No CGO.** Keep `CGO_ENABLED=0` builds working.

## Backend conventions

- Go 1.23+. `gofmt` + `goimports`; lint with `golangci-lint` (config in `.golangci.yml`).
- Errors: wrap with `fmt.Errorf("...: %w", err)`. Use sentinel/domain errors in `service` (`ErrNotFound`, `ErrConflict`, `ErrInvalid`, `ErrQuota`) and map them to HTTP status in `api` only.
- API errors always use the envelope in `01-api.md` (`{"error":{"code","message","details"}}`).
- Pass `context.Context` as the first parameter everywhere; honor cancellation.
- Logging: `log/slog`, structured, with `request_id`. Never log secrets, passwords, tokens, private keys, or `user_data`.
- SQL: write queries in `internal/store/queries/*.sql`, generate with sqlc. No string-concatenated SQL. Schema changes only via goose migrations (never edit an applied migration).
- IDs: `i-` + 17 hex (instances), `img-` + 17 hex, `key-` + 17 hex, `task-` + 17 hex, generated by `internal/id`. Users and internal rows use UUIDv7.
- Time: store UTC, RFC3339 in JSON.
- Config via env (`KUMO_*`) with sane defaults; document every var in `docs/design/00-architecture.md`.
- Tests: table-driven; use the fake hypervisor and an in-memory/temp SQLite DB. Handlers tested with `httptest`. Aim for tests alongside every new service method.

## Security rules (hard)

- Passwords: argon2id. JWT access token (15 min, HS256 or EdDSA, secret/key from config, auto-generated on first run and persisted with 0600 perms). Refresh token: random 256-bit, stored **hashed** in DB, rotated on use, delivered as `HttpOnly; Secure; SameSite=Strict` cookie.
- **Never store or log a generated private key.** It is returned exactly once in the create response.
- Validate and bound all input (names, sizes, URLs). Image import URLs: allow only `http(s)`, block link-local/metadata ranges by default (SSRF), enforce size limit and checksum when provided.
- All `/api/v1/*` routes except `/auth/login`, `/auth/refresh`, `/healthz` require a valid JWT.
- Libvirt domain XML must be built with `encoding/xml` structs or a template with escaped values. Never interpolate raw user input.
- VNC/serial consoles bind to `127.0.0.1` and are only reachable through the authenticated proxy.

## Frontend conventions

- TypeScript `strict: true`. No `any` (use `unknown` + narrowing). No default exports except route components.
- **Use Cloudscape components** (`@cloudscape-design/components`, `@cloudscape-design/collection-hooks`, `@cloudscape-design/global-styles`). Do not add another UI library or hand-roll what Cloudscape provides. Custom CSS only when unavoidable, via Cloudscape design tokens.
- Server state via **RTK Query** (`features/*/api.ts`); local UI state via component state; Redux slices only for genuinely global client state (auth session, flashbar notifications, preferences).
- Tables use `useCollection` (filtering, sorting, pagination, selection) + `CollectionPreferences`. Resource states use `StatusIndicator` with the mapping in `03-frontend.md`.
- Destructive actions require a confirmation `Modal` (terminate instance, delete image/key pair).
- Errors surface through `Flashbar`; forms show inline `FormField` errors from the API `details`.
- API types are generated from `api/openapi.yaml` (`npm run gen:api`); don't hand-write DTOs that duplicate it.
- Tests: Vitest + Testing Library; mock the network with MSW.
- Accessibility: keep Cloudscape a11y props (`ariaLabel`, `i18nStrings`) filled in.

## Git & PR workflow

- Branch per task: `m<milestone>/<short-name>`, e.g. `m2/keypairs-api`.
- Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `chore:`).
- Small PRs: one roadmap task each. Update docs and `openapi.yaml` in the same PR as behavior changes.
- Do not commit: `.env`, `*.db`, `bin/`, `web/dist/`, `node_modules/`, generated private keys or images.

## Definition of done

- [ ] Matches the task's acceptance criteria in `05-roadmap.md`
- [ ] `make lint test` pass
- [ ] New/changed endpoints reflected in `api/openapi.yaml` and `01-api.md`
- [ ] New/changed tables have a goose migration and are described in `02-data-model.md`
- [ ] Tests added (service + handler; component tests for new UI)
- [ ] No TODOs without an issue reference
- [ ] Verified error paths (not just the happy path)

## Ask before you…

- add a new dependency (Go module or npm package) beyond those named here
- change the stack, layering rules, ID formats, or auth design
- implement anything outside the current phase scope
- run destructive commands on the host (`virsh undefine`, deleting storage pools, `rm -rf` outside the repo)
- change an already-applied migration

## Things agents often get wrong here

- Forgetting that `go-libvirt` connections can drop. Use the reconnecting wrapper in `internal/hypervisor/libvirt`.
- Blocking HTTP handlers on slow libvirt/disk operations (use tasks).
- Leaving orphaned volumes/domains after a failed launch.
- Using AWS-specific names that don't fit (we mimic UX and vocabulary, **not** the AWS API; there is no EC2-compatible API in this phase).
- Reaching for localStorage for the access token; keep it in memory, and use the refresh cookie to restore the session.
