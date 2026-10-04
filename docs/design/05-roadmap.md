# 05 — Roadmap & Task Breakdown

Work through tasks **in order**, one per PR. Each task lists acceptance criteria (AC). Check off items as they land.

## M0 — Scaffolding

- [x] **M0.1 Repo init**: `go.mod` (`github.com/<you>/kumo`), Apache-2.0 `LICENSE`, `.gitignore`, `.editorconfig`, `.golangci.yml`, `Makefile` with all targets from `AGENTS.md`.
  AC: `make lint test build` succeed on an empty skeleton.
- [x] **M0.2 Backend skeleton**: `cmd/kumod`, config loader, slog, Gin router with `/healthz`, `/readyz`, `/api/v1/system/info`, graceful shutdown, request-id + recover + log middleware.
  AC: `curl :8080/healthz` → 200; unit test for config defaults.
- [x] **M0.3 Store**: SQLite open with pragmas, goose embedded migrations (`0001_init.sql` from `02-data-model.md`), sqlc config + first queries, test helper for temp DB.
  AC: migrations apply on empty DB; `make sqlc` clean diff.
- [x] **M0.4 Hypervisor interface + fake**: interface, fake impl with state machine, wiring by `KUMO_HYPERVISOR`.
  AC: fake unit tests cover start/stop/destroy/list and injected failures.
- [x] **M0.5 Frontend skeleton**: Vite + TS + Cloudscape + RTK + Router, `ConsoleLayout` (TopNavigation, SideNavigation, breadcrumbs, Flashbar) with placeholder pages, Vite proxy, Oxlint/Prettier/Vitest.
  AC: `npm run build` OK; layout renders with nav to Instances/Images/Key pairs.
- [x] **M0.6 Embed + OpenAPI**: `internal/webui` serves `web/dist` with SPA fallback; `api/openapi.yaml` derived from `01-api.md`; TS type generation.
  AC: `make build` yields a single binary serving the UI at `/`.

## M1 — Authentication

- [x] **M1.1 Backend auth**: argon2id hashing, `users` queries, `/auth/setup|login|refresh|logout|me`, JWT middleware, refresh rotation with reuse detection, login rate limit, role checks (`RequireRole`).
  AC: tests for happy path, wrong password (uniform error), expired/forged JWT, refresh reuse revokes family, setup only when no users.
- [x] **M1.2 CLI helpers**: `kumod admin create|reset-password`, `kumod version`, `kumod doctor` (stub).
  AC: reset-password works against an existing DB.
- [x] **M1.3 Login UI**: `/login`, `/setup`, `RequireAuth`, single-flight refresh baseQuery, sign-out in user menu.
  AC: component tests for login success/failure; reload keeps session via refresh cookie; 401 triggers redirect.

## M2 — Key pairs

- [x] **M2.1 Backend**: `internal/service/keypair` + handlers: create (ed25519/rsa-4096), import (validation), list, get, delete, tags; fingerprint SHA256; private key returned once and never persisted/logged.
  AC: tests for name validation/uniqueness, malformed/private-key-pasted import rejection, fingerprint matches `ssh-keygen -lf` for fixtures.
- [x] **M2.2 UI**: list, create (with private key modal, download .pem), import, delete confirm.
  AC: component tests; private key not retrievable after closing modal (no store persistence).

## M3 — Images

- [x] **M3.1 Libvirt infra**: real `hypervisor/libvirt` connect/reconnect, `EnsureInfra` (pools + network), `Ping`, `doctor` real checks.
  AC: integration test (tag `libvirt`) creates pools/network idempotently.
- [x] **M3.2 Image import backend**: task runner (workers, claim, restart recovery), `image.import` workflow (download, checksum, qcow2 inspect, place into pool), list/get/patch/delete, catalog endpoint, SSRF guard.
  AC: tests with `httptest` server serving a tiny qcow2; failure cleans up temp + volume; delete blocked when in use.
- [x] **M3.3 Images UI**: list with progress, import form with catalog, detail page, delete.
  AC: progress bar updates via polling; error state shows reason.
  Note: `progress` is 0 for a chunked download (no `Content-Length`), so the row has to show the image state until a percentage exists.

## M4 — Instances (core)

- [x] **M4.1 Instance types + capacity**: seed types, `GET /instance-types`, `GET /system/capacity`, capacity check.
  AC: the capacity rules are documented in `00-architecture.md` §6; a launch check and the capacity report share one implementation.
- [ ] **M4.2 Cloud-init + domain XML builders**: pure functions with golden-file tests (`service/cloudinit`, `hypervisor/libvirt/domainxml`), ISO9660 seed builder.
  AC: golden tests; cloud-init merge cases covered (no user data, cloud-config, script).
- [ ] **M4.3 Launch workflow**: `instance.launch` task per `04-libvirt-integration.md §3`, rollback on failure, IP discovery.
  AC: fake-hypervisor tests for success and each failure step leaving no orphans; real-libvirt integration test boots CirrOS.
- [ ] **M4.4 Lifecycle actions**: start/stop/reboot/terminate, per-resource locking (`409` on conflict), console-output endpoint.
- [ ] **M4.5 Reconciler + events**: periodic reconcile, lifecycle event hints, orphan report.
  AC: killing a VM via `virsh destroy` is reflected in the UI within ~10 s.
- [ ] **M4.6 Instances UI**: list with state actions, launch form, detail page with tabs, polling, terminate confirmation.
  AC: component tests; manual E2E against real KVM documented in `docs/testing.md`.

## M5 — Polish for first release (v0.1)

- [ ] VNC web console (WebSocket proxy to `127.0.0.1:<vnc>`, noVNC in UI)
- [ ] Dashboard page (state counts, capacity, recent tasks)
- [ ] SSE `/events` + UI subscription (fallback polling)
- [ ] Tags editor everywhere; viewer role enforced in UI (hide/disable actions)
- [ ] TLS: built-in self-signed cert generation or `KUMO_TLS_CERT/KEY`
- [ ] Install script + systemd unit + `.deb`/`.rpm` via goreleaser; `docs/install.md` (the 15-minute path)
- [ ] Playwright E2E on fake hypervisor; GitHub Actions CI (lint, test, build, release)
- [ ] `CONTRIBUTING.md`, `SECURITY.md`, issue templates

## Post-v0.1 backlog (do not start without approval)

Volumes & snapshots · security groups (nwfilter) · bridged/VLAN networks · SSH-key rotation · instance resize · multi-host · S3 (RustFS) · RDS-like (Postgres/MySQL) · Redis · NATS/ActiveMQ · OpenBao secrets · Kubernetes clusters · metrics (Prometheus) · audit log · OIDC login.

## Open questions

1. **Project name & domain** ("Kumo" is a placeholder).
2. **Networking default**: NAT-only vs. offering bridged on first run?
3. **Multi-user model**: stay admin/viewer, or introduce projects early (cheaper than retrofitting)?
4. **Image catalog**: ship a static JSON in-repo, or fetch signed catalog from a URL?
5. **JWT algorithm**: HS256 with local secret (simple) vs EdDSA (enables future service-to-service verification).
