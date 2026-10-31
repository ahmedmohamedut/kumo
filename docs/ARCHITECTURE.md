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
