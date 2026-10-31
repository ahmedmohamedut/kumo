# Architecture — kumo

## Overview

Kumo is an open-source, AWS-style private cloud for teams that find OpenStack too heavy and Proxmox too low-level. One Go binary, one web console, one SQLite file. No message bus cluster, no dozen microservices.

The full design lives in `docs/design/` (start with `00-architecture.md`); this file is the
summary the platform's agents are pointed at. If the two disagree, `docs/design/` and
`AGENTS.md` win — flag the disagreement rather than diverging.

## Stack

- **Language:** Go (backend, module `github.com/ahmedmohamedut/kumo`), TypeScript (web console)
- **HTTP:** Gin · **Database:** SQLite (`modernc.org/sqlite`, pure Go, no CGO) via sqlc + goose migrations
- **Hypervisor:** KVM/libvirt through `go-libvirt` behind the `Hypervisor` interface (a fake implementation for tests and dev)
- **Object storage:** S3 via RustFS, only inside `internal/objectstore`
- **Frontend:** React + Redux Toolkit (RTK Query) + Vite + Cloudscape Design System
- **Build / test:** `make build`, `make test` (`go test ./... -race` + web tests), `make lint`

## Module structure

```
cmd/kumod/          main(): wiring only, no logic
internal/
  api/              Gin router, middleware, handlers, DTOs
  auth/             password hashing, JWT, refresh tokens
  service/          business logic (instances, images, keypairs, users, tasks)
  hypervisor/       Hypervisor interface + libvirt impl + fake impl
  objectstore/      ObjectStore interface + RustFS impl + fake impl
  store/            sqlc queries, migrations, transactions
  worker/           async task runner + reconciler loop
  config/ id/ logging/ network/ webui/ ...
api/openapi.yaml    API source of truth
web/src/            React console (app/, components/, features/, lib/, types/)
docs/design/        design docs
```

## Dependency rules

- Layering: `api` -> `service` -> (`store`, `hypervisor`). Handlers never call libvirt or SQL
  directly. Services never import `gin`. HTTP status mapping happens in `api` only.
- All libvirt access goes through the `Hypervisor` interface; no `virsh` shelling out, no cgo.
- libvirt is the source of truth for runtime state, SQLite for metadata; a reconciler syncs them.
- Long operations are asynchronous: the handler creates a task and returns `202`.
- Single binary, zero external services at runtime. No cgo (`CGO_ENABLED=0` builds must work).
- No circular dependencies between packages.

<!-- gestalt:architecture feature=e8e5ffd8-8d95-4824-be5d-8a1a6f74b301 START -->
## Feature: Verification: id.ResourcePrefix

### Summary
Adds a pure utility function `ResourcePrefix(value string) (string, bool)` to `internal/id/resource.go`. It extracts the resource type prefix from a well-formed resource ID by delegating to the existing `HasResourceShape` function and the seven declared `Prefix*` constants.

### Domain Impact
- **Entity**: `ResourceID` (existing). No new entities or lifecycle states introduced.
- **Business Rule**: `ResourcePrefix` strictly validates input against existing shape rules. Returns `(prefix, true)` on match, `("", false)` otherwise.

### Data & Persistence
- No SQL schemas, repositories, or transaction contracts required. This is a stateless utility addition.

### Application & Modules
- **Module**: `internal-id` (`internal/id`)
- **Changes**:
  - `internal/id/resource.go`: Append `ResourcePrefix` function.
  - `internal/id/resource_test.go`: Append table-driven tests covering valid prefixes, unknown prefixes, length mismatches, uppercase hex, and empty strings.
- **Dependencies**: None. Pure function, no external imports beyond standard library and existing package internals.

### Contracts
- `authContract`: N/A (No API endpoints)
- `errorResponseContract`: N/A (No API endpoints)
- `transactionContract`: N/A (No database writes)

### Recommended Phases
1. **Implement `ResourcePrefix` and Tests**: Add the function to `resource.go` and corresponding table-driven tests to `resource_test.go`. Verify stack compliance (Go 1.26, Gin/SQLite monolith context) and ensure no regression in existing `HasResourceShape` logic.
<!-- gestalt:architecture feature=e8e5ffd8-8d95-4824-be5d-8a1a6f74b301 END -->
