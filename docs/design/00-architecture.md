# 00 — Architecture

## 1. Goals and non-goals

**Goals**
- Run a private cloud on one or more KVM hosts with a 15-minute install.
- AWS-like vocabulary and console UX (instances, images, key pairs, tags, instance types).
- Single static binary + SQLite. Operationally boring.

**Non-goals (this phase)**
- Multi-node scheduling / clustering (design so it's possible later; ship single-node first)
- EC2 API compatibility
- Multi-tenancy / IAM-style policies (just `admin` and `viewer` roles)
- Software-defined networking, security groups, load balancers, volumes (EBS-like) as separate resources
- Live migration, snapshots, GPU passthrough

## 2. System overview

```
 Browser (React + Cloudscape)
        │  HTTPS  /api/v1/*  (JWT)         static assets (go:embed)
        ▼
 ┌──────────────────────────── kumod (single Go binary) ───────────────────────────┐
 │  Gin router ─ middleware (request-id, log, recover, auth)                         │
 │      │                                                                            │
 │  handlers (internal/api)  ── DTO validation, status mapping                       │
 │      │                                                                            │
 │  services (internal/service) ── business rules, workflows                         │
 │      │                      │                                                    │
 │  store (SQLite, sqlc)    hypervisor.Hypervisor (interface)                        │
 │                              ├─ libvirt impl (go-libvirt, unix socket)           │
 │                              └─ fake impl (tests / dev without KVM)              │
 │                                                                                   │
 │  worker: task runner (goroutines + DB-backed queue)  +  reconciler (10s loop)    │
 └────────────────────────────────────────┬──────────────────────────────────────────┘
                                          │ /var/run/libvirt/libvirt-sock
                                          ▼
                                libvirtd → QEMU/KVM guests
                                Storage pools: kumo-images, kumo-instances
                                Network: libvirt NAT network "kumo-default"
```

## 3. Key design decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Single binary, embedded SPA | Fits the 15-minute install goal |
| D2 | SQLite (WAL mode, `modernc.org/sqlite`) | Zero ops, no CGO; ample for one control plane |
| D3 | libvirt is the runtime source of truth; DB is metadata truth; reconciler syncs | Survives out-of-band `virsh` changes and crashes |
| D4 | Hypervisor interface | Testability, future multi-host and other backends |
| D5 | Async tasks persisted in DB | Resume/mark-failed after restart; UI can poll |
| D6 | cloud-init NoCloud seed ISO per instance | Works with any cloud image, no metadata service needed in v1 |
| D7 | qcow2 backing-file root disks | Instant, space-efficient launches |
| D8 | REST + polling first; SSE later | Simplest thing that works |
| D9 | sqlc + goose | Type-safe SQL, explicit migrations, agent-friendly |
| D10 | Access JWT in memory + refresh cookie | Limits XSS impact of token theft |

## 4. Concurrency & state model

- **Instance state machine** (DB column `state`):
  `pending → running ⇄ stopping → stopped → running`, `* → terminating → terminated`, `* → error`.
  Transitional states (`pending`, `stopping`, `terminating`) are owned by a task. Steady states are observed from libvirt.
- **Tasks:** table `tasks` (`pending|running|succeeded|failed`). Worker pool (default 4) claims tasks with an atomic `UPDATE ... WHERE state='pending'`. On startup, tasks left `running` are marked `failed` (or retried if the type is idempotent) and the owning resource moves to `error` or is reconciled.
- **Per-resource lock:** an in-process keyed mutex (`internal/worker/locks`) serializes operations on the same instance/image ID. Reject conflicting actions with `409`.
- **Reconciler (every 10 s, plus libvirt lifecycle events):**
  1. List domains from libvirt, match by name == instance ID.
  2. DB instance without a domain and not `pending`/`terminated` → mark `error` (`domain_missing`).
  3. Domain without a DB row → log and expose in `GET /system/orphans` (do not auto-delete in v1).
  4. Update observed state and private IP (`DomainInterfaceAddresses`, lease source, with ARP fallback).

## 5. Configuration (env vars)

| Variable | Default | Description |
|---|---|---|
| `KUMO_LISTEN` | `:8080` | HTTP listen address |
| `KUMO_DATA_DIR` | `/var/lib/kumo` | SQLite DB, secrets, uploaded files |
| `KUMO_DB_PATH` | `$DATA_DIR/kumo.db` | SQLite file |
| `KUMO_HYPERVISOR` | `libvirt` | `libvirt` or `fake` |
| `KUMO_LIBVIRT_URI` | `unix:///var/run/libvirt/libvirt-sock` | libvirt socket |
| `KUMO_POOL_IMAGES` | `kumo-images` | libvirt storage pool for base images |
| `KUMO_POOL_INSTANCES` | `kumo-instances` | pool for root disks and seed ISOs |
| `KUMO_NETWORK` | `kumo-default` | libvirt network for instance NICs |
| `KUMO_JWT_SECRET_FILE` | `$DATA_DIR/jwt.key` | auto-generated if missing (0600) |
| `KUMO_ACCESS_TTL` | `15m` | access token lifetime |
| `KUMO_REFRESH_TTL` | `168h` | refresh token lifetime |
| `KUMO_COOKIE_SECURE` | `true` | set `false` for plain-HTTP dev |
| `KUMO_ALLOWED_ORIGINS` | *(empty)* | dev CORS (Vite at `http://localhost:5173`) |
| `KUMO_WORKERS` | `4` | task worker count |
| `KUMO_IMAGE_MAX_BYTES` | `20GiB` | max image import size |
| `KUMO_IMAGE_TEMP_DIR` | `$KUMO_DATA_DIR/staging` | where a download is staged; must share a filesystem with the images pool |
| `KUMO_IMAGE_ALLOW_PRIVATE_URLS` | `false` | allow imports from loopback/private/link-local hosts (trusted LAN mirrors) |
| `KUMO_MEMORY_RESERVE_MIB` | `1024` | host memory kept out of the pool of allocatable RAM |
| `KUMO_VCPU_OVERCOMMIT` | `4` | vCPUs offered per host vCPU in `GET /system/capacity` and the launch check |
| `KUMO_LOG_LEVEL` | `info` | `debug|info|warn|error` |

## 6. Capacity accounting

`GET /system/capacity` reports what the node has against what it has handed out, per
metric as `{total, allocated}`:

| Metric | `total` | `allocated` |
|---|---|---|
| vCPUs | host vCPUs × `KUMO_VCPU_OVERCOMMIT` | Σ `instances.vcpus` |
| memory | host RAM from libvirt | Σ `instances.memory_mib` |
| disk | size of `KUMO_POOL_INSTANCES` | Σ `instances.root_disk_gb` |

The split is deliberate:

- **`total` comes from the hypervisor, `allocated` from SQLite.** libvirt knows the
  node and the pool but not whether a disk backing an overlay was written; SQLite knows
  what was asked for but can drift. Summing the request keeps the report equal to what
  the launch check reserved, which is the number that must never be exceeded.
- **`allocated` counts every instance whose state is not `terminated`.** A terminated
  row is kept for history and holds nothing, so it must stop consuming quota or a node
  could never launch again after enough terminations.
- **Free is derived, never stored.** `Free = total − allocated`, less
  `KUMO_MEMORY_RESERVE_MIB` for memory so the host keeps room for itself. The same
  arithmetic backs the launch check in `internal/service/capacity`, so what the console
  shows and what a launch accepts cannot drift apart.

Both sums are vCPU-overcommitted CPU, so `allocated > total` is possible by design on
memory- or disk-bound nodes; the endpoint reports that rather than hiding it.

## 7. Startup sequence

1. Load config, open SQLite (WAL, `busy_timeout=5000`, `foreign_keys=ON`), run embedded goose migrations.
2. Ensure JWT key exists.
3. Connect to hypervisor; **bootstrap**: ensure storage pools and network exist (create if missing: dir pools under `$DATA_DIR/pools/*`, NAT network `192.168.100.0/24` named `kumo-default`).
4. Create the staging directory (`$KUMO_IMAGE_TEMP_DIR`), where image downloads land before they become volumes.
5. Seed the `kumo.*` `instance_types` presets. Seeding is idempotent (`INSERT OR IGNORE`), so a preset an operator tuned on the node survives a restart.
6. Start worker + reconciler. The worker first *recovers* tasks left `running` by a previous process (back to `pending`, attempts preserved) and only then starts claiming.
7. Start HTTP server. If no users exist, `/api/v1/system/info` reports `needs_setup: true`; the UI shows first-run admin creation (`POST /auth/setup`, only allowed when zero users exist).

## 8. Error model

Domain errors in `service` map to HTTP: `ErrInvalid→400/422`, `ErrUnauthorized→401`, `ErrForbidden→403`, `ErrNotFound→404`, `ErrConflict→409`, anything else `500` (message hidden, request ID logged).

## 9. Observability

- `slog` JSON logs with `request_id`, `user_id`, `resource_id`, `task_id`.
- `GET /healthz` (liveness), `GET /readyz` (DB + hypervisor reachable).
- Prometheus metrics at `/metrics` (admin-only or loopback) — post-MVP.

## 10. Future-proofing (don't build now, don't preclude)

- Multi-host: `hosts` table, `Hypervisor` per host, scheduler picks host.
- Other services (S3, RDS, …) as modules registered under `/api/v1/<service>` and nav groups in the console.
- Tags on every resource, via the generic `tags` table.
