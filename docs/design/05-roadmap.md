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
- [x] **M4.2 Cloud-init + domain XML builders**: pure functions with golden-file tests (`service/cloudinit`, `hypervisor/libvirt/domainxml`), ISO9660 seed builder.
  AC: golden tests; cloud-init merge cases covered (no user data, cloud-config, script).
  Done: `internal/service/cloudinit` renders meta-data, network-config and user-data (generated default user, verbatim pass-through, cloud-config merge, MIME for a script, duplicate-key and malformed-YAML refusals). `internal/hypervisor/libvirt/domainxml` builds the domain XML from `encoding/xml` structs with golden files; `internal/seediso` writes the `cidata` image and is tested by reading the ISO back.
- [x] **M4.3 Launch workflow**: `instance.launch` task per `04-libvirt-integration.md §3`, rollback on failure, IP discovery.
  AC: fake-hypervisor tests for success and each failure step leaving no orphans; real-libvirt integration test boots CirrOS.
  Done: `internal/service/instance` validates a launch against the capacity report of `00-architecture.md §6` (the same arithmetic the capacity endpoint uses), writes the row and its task in one transaction, and hands the boot to the worker (`internal/service/instance/workflow.go`). The workflow creates the root disk, the seed ISO and the domain, polls for an address (DHCP lease first, qemu-guest-agent second), and records the uuid, MAC and address with the state; a failure at any step removes the domain and the volumes it created and leaves the row in `error` with the reason. Reads (`GET /instances`, `GET /instances/{id}`) are done, `user_data` reaching only admins, `POST /instances` is admin-only and answers `202` with a `Location` header for the task.
  Verified: fake-hypervisor tests for the success path, for a failure injected at each of the four steps (nothing left behind in either pool), for a canceled launch (left `pending` for the requeued task, nothing deleted), for a guest that never takes an address, and for a write that fails while the guest runs; handler tests over `httptest` for `202`/`400`/`403`/`401`/`404`, pagination tokens and the hidden `user_data`; unit tests for the libvirt state mapping, address filtering, uuid rendering and memory units against real libvirt output shapes.
  Not yet run here: `TestLaunchBootsCirrOS` (`make test-libvirt` with `KUMO_TEST_CIRROS_URL` pointing at a CirrOS cloud image) imports an image, boots a guest on the Kumo network and waits for its DHCP address against a real libvirtd. It is written and compiles under `-tags libvirt`, but no run against real hardware has happened yet; that leg of the AC is still open.
- [x] **M4.4 Lifecycle actions**: start/stop/reboot/terminate, per-resource locking (`409` on conflict), console-output endpoint.
  AC: written after the work, so it reads as the check that was applied and not as a promise it was measured against: fake-hypervisor tests for each action and for the conflict paths (busy instance, wrong state, repeated terminate); handler tests over `httptest` for `202`/`400`/`403`/`404`/`409`/`501`.
  Done: `internal/service/instance/lifecycle.go` queues one task per action (`instance.start`, `instance.stop`, `instance.reboot`, `instance.terminate`) writing the state the action implies in the same transaction, guarded by a per-instance lock (`internal/worker/locks`), by the states of `00-architecture.md §4` and by the latest task of the instance; each of those three answers `409` with a message saying what to do instead. The workflows hold the same lock, so a launch cannot rebuild what a terminate is deleting, and they tolerate every resource being already gone, which is what makes a requeued task able to finish. A graceful stop asks the guest through ACPI and waits, bounded by `KUMO_STOP_TIMEOUT` (90 s) before failing and leaving the row `stopping` for a forced stop; `?force=true` destroys or resets instead of asking. `GET /instances/{id}/console-output` serves the tail of the log libvirt writes for the domain, resolved from the domain's own `<log>` element; `GET /instances/{id}/console` answers `501` until M5. `PATCH /instances/{id}` renames and re-tags. The libvirt backend implements `Start`, `Shutdown`, `Reboot`, `Reset` and `ConsoleOutput` against `DomainCreate`, `DomainShutdown`, `DomainReboot`, `DomainReset` and a bounded read of the serial log; `Events` still refuses, and stays with M4.5.
  Verified: fake-hypervisor tests for start, graceful stop, forced stop, the watchdog on a guest that ignores ACPI, reboot, forced reboot, terminate (domain and both volumes gone, the image volume untouched, the name released), a repeated terminate, a terminate of a stopped instance, a terminate whose domain an operator already removed, a launch that skips an instance on its way out, an instance whose domain libvirt does not know (`error` with `domain_missing: `), and the three guards; handler tests for the state walk, `409` on a second action, `403` for a viewer, a `400` for `?force=maybe`, the console tail and its `400`/`404`/`501`, and the rename/retag with its refusals. A terminate removes the instance rather than marking it: the domain, its storage and its NVRAM file go, then the tags and the row in one transaction (the interface rows follow through the cascade), then the console log — so `DeleteInstance` runs at the end of every terminate, `GET /instances/{id}` answers `404` after it, and a `DELETE` for a row an earlier release left in `terminated` purges that row.
  Not yet run here: `TestLaunchBootsCirrOS` (`make test-libvirt` with `KUMO_TEST_CIRROS_URL`) still has never been run against a real libvirtd. It now also drives the M4.4 power calls and the console log against that live domain, so the libvirt half of this task is written but unexercised on hardware; the AC for this task is fake-hypervisor and handler coverage, which is done.
- [ ] **M4.5 Reconciler + events**: periodic reconcile, lifecycle event hints, orphan report.
  AC: killing a VM via `virsh destroy` is reflected in the UI within ~10 s.
- [x] **M4.6 Instances UI**: list with state actions, launch form, detail page with tabs, polling, terminate confirmation.
  AC: component tests; manual E2E against real KVM documented in `docs/testing.md`.
  Done: `web/src/features/instances` follows the same shape as the images feature — `api.ts` (`injectEndpoints` on the shared `kumoApi`, so the invalidation graph is common), `state.ts` for the `StatusIndicator` mapping, `columns.tsx`, and the pages. `InstancesListPage` is a `useCollection` table with the server-side `?q=` filter kept in the URL, a single-selection `ButtonDropdown` whose entries are disabled by the selected instance's state, and the refresh and **Launch instance** actions; `LaunchInstancePage` is a `Form` of `Container`s with the name, image, instance type, key pair, root disk, cloud-init and a live summary, validating locally before spending a request; `InstanceDetailPage` is a `ContentLayout` with **Details**, **Storage**, **Tags**, **Console output** and **Tasks** tabs and the same lifecycle actions, because an operator watching a boot wants Stop where they already are. Polling follows the data (5 s while anything is `pending`/`stopping`/`terminating`, 30 s once settled, `running` deliberately not transitional). The guard table in `actions.ts` transcribes `internal/service/instance/lifecycle.go` so a button is not live only to return a `409`; the instance mocks enforce the same table.
  Verified: 34 component tests (Vitest + Testing Library + MSW) over the list, the launch form and the detail page — the state cell carrying `state_reason`, a terminated row kept as history, the empty state, the URL filter, each action gated by state, `202` worded as accepted rather than finished, the terminate modal refusing a wrong name and the force-stop modal not asking for one, the launch form refusing a bad name, a too-small root disk and a duplicate name before any request, and the detail page's alert for an `error` instance, its tags, its console tail and its `404`. `npm run typecheck`, `oxlint` and `prettier --check` are clean, and the whole frontend suite (157 tests) passes.
  Not yet run here: the manual E2E is written up in `docs/testing.md` §3 — import, launch, confirm the guest has a DHCP address and a boot log, stop/start/reboot, terminate, and the image-delete `409` — but it has never been executed against a real libvirtd, and `TestLaunchBootsCirrOS` (`make test-libvirt`) is still unexercised on hardware. The component-test half of the AC is done; the manual-KVM half is documented and open.

## M4b — VPCs & Subnets (networking)

- [x] **M4b.1 VPC & Subnet data model and libvirt infra**: migration for `vpcs`, `subnets`, `network_interfaces` tables; `Hypervisor` interface methods for VPC network define/start/destroy/undefine and static DHCP host add/delete; bridge naming (`kmb<8-hex>`); default network adoption; host route collision detection.
  AC: unit tests for CIDR validation (RFC 1918, /16–/28, no overlap), host route collision scanner, bridge name generator; fake hypervisor implements network operations.
- [x] **M4b.2 VPC & Subnet service and API**: `internal/service/vpc` and `internal/service/subnet` with full CRUD; immutability checks (`cidr`, `mode`, `name`); transaction-level consistency for delete vs launch; openapi.yaml schemas.
  AC: handler tests for VPC/Subnet endpoints; `409` on overlap and non-empty delete; viewer/admin role enforcement.
- [x] **M4b.3 Instance integration (static DHCP & ENI)**: wire instance launch to allocate IP/MAC from subnet in launch transaction, write static DHCP host entry before domain boot, clean up ENI and DHCP host on terminate; instance launch accepts optional `subnet_id`.
  AC: fake hypervisor verifies static DHCP host entry created before start and deleted on terminate; address and MAC stable across stop/start; instance list/detail includes IP and interface info.
- [x] **M4b.4 VPC & Subnet UI**: VPC list/detail/create/delete; Subnet list/detail/create/delete; launch instance form updated with VPC/subnet selector.
  AC: component tests for VPC and Subnet pages; subnet selector in launch form; delete modals with resource-in-use guards.

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
