# 00 — Architecture

## 1. Goals and non-goals

**Goals**
- Run a private cloud on one or more KVM hosts with a 15-minute install.
- AWS-like vocabulary and console UX (instances, images, key pairs, VPCs, subnets, tags, instance types, and S3-compatible buckets and access keys).
- Single static binary + SQLite. Operationally boring. Optional modules that need an outside server (object storage on RustFS) are off until configured.

**Non-goals (this phase)**
- Multi-node scheduling / clustering (design so it's possible later; ship single-node first)
- EC2 API compatibility
- Multi-tenancy / IAM-style policies (just `admin` and `viewer` roles)
- Advanced software-defined networking, security groups (nwfilter), load balancers, volumes (EBS-like) as separate resources
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
                                Networks: VPC libvirt networks (one per VPC, bridge kmb<8-hex>)
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
| D11 | One VPC = one libvirt network = one Linux bridge (`kmb` + 8 hex of VPC ID) | VPCs isolated by libvirt forwarding rules; subnets are logical DB partitions |
| D12 | Static DHCP host entries; Kumo is sole IP allocator | dnsmasq only serves what Kumo assigns; IP/MAC stable across stop/start |

## 4. Concurrency & state model

- **Instance state machine** (DB column `state`):
  `pending → running ⇄ stopping → stopped → running`, `* → terminating → (row deleted)`, `* → error`.
  Transitional states (`pending`, `stopping`, `terminating`) are owned by a task. Steady states are observed from libvirt. A terminate ends in the row being deleted rather than in a state: `terminated` is still a legal value only because rows an earlier release kept as history have to keep rendering and be purged.
- **Tasks:** table `tasks` (`pending|running|succeeded|failed`). Worker pool (default 4) claims tasks with an atomic `UPDATE ... WHERE state='pending'`. On startup, tasks left `running` are marked `failed` (or retried if the type is idempotent) and the owning resource moves to `error` or is reconciled.
- **Per-resource lock:** an in-process keyed mutex (`internal/worker/locks`) serializes operations on the same instance/image/VPC ID. Reject conflicting actions with `409`. A request takes the lock without waiting (so an operator pressing stop twice is told the instance is busy); a task waits for it, because two queued tasks for one instance are going to run in some order. An unfinished task row counts as busy too, since a task killed with the process holds no lock until it is requeued. Tasks operating on VPCs serialize on `vpc.LockKey(id)`.
- **Consistency from transactions, not locks:** A state check and its write share one SQLite transaction (single writer via WAL mode), so a launch cannot slip into a VPC or subnet that a delete has just checked.
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
| `KUMO_NETWORK` | `kumo-default` | default libvirt network adopted on first run if present |
| `KUMO_DEFAULT_VPC_CIDR` | `192.168.100.0/24` | range of the default VPC a fresh boot creates; must be RFC 1918 and must not overlap any host LAN route (a NAT guest network never shadows the operator's own network), set this when your LAN already owns the default |
| `KUMO_JWT_SECRET_FILE` | `$DATA_DIR/jwt.key` | auto-generated if missing (0600) |
| `KUMO_ACCESS_TTL` | `15m` | access token lifetime |
| `KUMO_REFRESH_TTL` | `168h` | refresh token lifetime |
| `KUMO_COOKIE_SECURE` | `true` | set `false` for plain-HTTP dev |
| `KUMO_ALLOWED_ORIGINS` | *(empty)* | dev CORS (Vite at `http://localhost:5173`) |
| `KUMO_WORKERS` | `4` | task worker count |
| `KUMO_STOP_TIMEOUT` | `90s` | how long a graceful stop waits for the guest before the task fails and the instance stays `stopping` |
| `KUMO_IMAGE_MAX_BYTES` | `20GiB` | max image import size, of the download and again of a gzipped download once decompressed |
| `KUMO_IMAGE_TEMP_DIR` | `$KUMO_DATA_DIR/staging` | where a download is staged; must share a filesystem with the images pool |
| `KUMO_IMAGE_ALLOW_PRIVATE_URLS` | `false` | allow imports from loopback/private/link-local hosts (trusted LAN mirrors) |
| `KUMO_MEMORY_RESERVE_MIB` | `1024` | host memory kept out of the pool of allocatable RAM |
| `KUMO_VCPU_OVERCOMMIT` | `4` | vCPUs offered per host vCPU in `GET /system/capacity` and the launch check |
| `KUMO_LOG_LEVEL` | `info` | `debug|info|warn|error` |
| `KUMO_OBJECTSTORE` | `none` | object storage module: `none`, `rustfs` or `fake`; the `KUMO_S3_*` variables below apply only when it is on |
| `KUMO_S3_ENDPOINT` | *(required with `rustfs`)* | where kumod reaches the S3 API |
| `KUMO_S3_PUBLIC_URL` | `$KUMO_S3_ENDPOINT` | where browsers reach it; presigned URLs are signed for this host |
| `KUMO_S3_REGION` | `us-east-1` | signed into every S3 request |
| `KUMO_S3_CREDENTIALS_FILE` | `$KUMO_DATA_DIR/s3.credentials` | RustFS admin keys, mode `0600` or stricter |
| `KUMO_S3_CORS_ORIGINS` | *(empty)* | console origins allowed to use presigned URLs |
| `KUMO_S3_PRESIGN_TTL` | `15m` | presigned URL lifetime, at most `168h` |
| `KUMO_DATABASES` | `on` | managed Postgres module: `on` or `off`; with `off` every `/api/v1/rds/*` route answers `503` and the console hides the Databases pages |
| `KUMO_DB_PROBE_INTERVAL` | `10s` | how often the readiness probe dials a creating or running DB instance |
| `KUMO_DB_READY_TIMEOUT` | `10m` | how long a `db.create` task waits for the engine before the DB instance is `failed`; must be longer than the probe interval |
| `KUMO_KUBERNETES` | `on` | Kubernetes clusters module: `on` or `off`; with `off` every `/api/v1/eks/*` route answers `503` and the console hides the Kubernetes pages (`08-kubernetes.md`) |
| `KUMO_K8S_READY_TIMEOUT` | `20m` | how long each wait of a `k8s.create` lasts -- the API server, then every node Ready -- before the cluster is `failed` or `degraded` |
| `KUMO_K8S_PROBE_INTERVAL` | `30s` | how often the health pass asks an active or degraded cluster's API server |
| `KUMO_SPARK` | `on` | Spark clusters module: `on` or `off`; with `off` every `/api/v1/spark/*` route answers `503` and the console hides the Spark pages (`09-spark.md`) |
| `KUMO_SPARK_READY_TIMEOUT` | `10m` | how long each wait of a `spark.create` lasts -- the master, then every worker registered -- before the cluster is `failed` or `degraded` |
| `KUMO_SPARK_PROBE_INTERVAL` | `30s` | how often the health pass reads an active or degraded cluster's master |
| `KUMO_SPARK_DRAIN_TIMEOUT` | `10m` | how long a scale-down lets one worker decommission before its VM is forced off (`09-spark.md` S13) |
| `KUMO_SPARK_S3_ENDPOINT` | `$KUMO_S3_PUBLIC_URL` | where the Spark guests reach the S3 server; must be routable from a VPC, so never a loopback address (`09-spark.md` section 2) |

The object storage variables are explained in `06-object-storage.md` §2, the
database ones in `07-databases.md` §2. Unlike object storage, the databases
module is **on by default**: it needs no server Kumo does not ship, so there is
nothing for an operator to turn on. What it does need is an engine image, which
is a state `/system/info` reports rather than a configuration error.

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
- **`allocated` counts every instance whose state is not `terminated`, and a terminate removes the row instead of writing that state**, so the report stops summing an instance the moment it is gone. Rows left in `terminated` by an earlier release hold nothing and are purged by the next `DELETE` for their id.
- **Free is derived, never stored.** `Free = total − allocated`, less
  `KUMO_MEMORY_RESERVE_MIB` for memory so the host keeps room for itself. The same
  arithmetic backs the launch check in `internal/service/capacity`, so what the console
  shows and what a launch accepts cannot drift apart.

Both sums are vCPU-overcommitted CPU, so `allocated > total` is possible by design on
memory- or disk-bound nodes; the endpoint reports that rather than hiding it.

## 7. Startup sequence

1. Load config, open SQLite (WAL, `busy_timeout=5000`, `foreign_keys=ON`), run embedded goose migrations. Migration `0003` seeds the `kumo.*` `instance_types` presets once; nothing seeds them at boot, because administrators manage the set and a boot-time seed would bring back a deleted preset.
2. Ensure JWT key exists.
3. Connect to hypervisor; **bootstrap**: ensure storage pools exist (dir pools under `$DATA_DIR/pools/*`). Ensure default VPC and subnet exist in DB and libvirt; if an existing `kumo-default` network is found, adopt it (storing its existing network and bridge names); otherwise create the default VPC network (`kmb` + 8 hex ID) with collision validation against host routes/interfaces. A collision is **not fatal**: the boot logs a warning and continues with no default VPC (`vpc.ErrDefaultSkipped`); the first VPC an operator creates then becomes the default and its first subnet the default subnet, and `kumod doctor` and the console dashboard warn about the missing default.
4. Create the staging directory (`$KUMO_IMAGE_TEMP_DIR`), where image downloads land before they become volumes.
5. Start worker + reconciler. The worker first *recovers* tasks left `running` by a previous process (back to `pending`, attempts preserved) and only then starts claiming.
6. Start HTTP server. If no users exist, `/api/v1/system/info` reports `needs_setup: true`; the UI shows first-run admin creation (`POST /auth/setup`, only allowed when zero users exist).

## 8. Error model

Domain errors in `service` map to HTTP: `ErrInvalid→400/422`, `ErrUnauthorized→401`, `ErrForbidden→403`, `ErrNotFound→404`, `ErrConflict→409`, anything else `500` (message hidden, request ID logged).

## 9. Observability

- `slog` JSON logs with `request_id`, `user_id`, `resource_id`, `task_id`.
- `GET /healthz` (liveness), `GET /readyz` (DB + hypervisor reachable).
- Prometheus metrics at `/metrics` (admin-only or loopback) — post-MVP.

## 10. Future-proofing (don't build now, don't preclude)

- Multi-host: `hosts` table, `Hypervisor` per host, scheduler picks host.
- Other services (S3, RDS, …) as modules registered under `/api/v1/<service>` and nav groups in the console. Object storage is the first (`06-object-storage.md`, shipped in v0.2.0); managed Postgres is the second (`07-databases.md`, M7, built); Kubernetes clusters are designed as the third (`08-kubernetes.md`, M8).
- A module may depend on a server Kumo does not ship, as object storage depends on RustFS. Such a module is off by default, so the single-binary, zero-external-services install stays true for anyone who does not turn it on. A server that is down never stops kumod or fails `/readyz`; it is reported in `/system/info` and by `kumod doctor`.
- Tags on every resource, via the generic `tags` table.
