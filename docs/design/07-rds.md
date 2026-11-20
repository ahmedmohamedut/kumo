# 07 — Managed databases (RDS-like, PostgreSQL)

Kumo's second service beyond compute. A **database instance** is a PostgreSQL server that
Kumo provisions, watches and deletes for you, with an endpoint, a master user and a place in a VPC.
It is the "modules registered under `/api/v1/<service>`" extension point of
`00-architecture.md` §10, and the AWS RDS vocabulary the console mimics.

Where object storage (`06-object-storage.md`) is a control plane over a server Kumo does not ship,
this module is a control plane over **Kumo's own compute**: a database instance is a virtual
machine Kumo launches the way it launches any instance, with a data disk and a cloud-init script
that installs and configures PostgreSQL.

## 0. Status

**PROPOSED. Not approved, not built.** `05-roadmap.md` lists RDS-like databases in the post-v0.1
backlog ("do not start without approval"). This document exists so the owner can approve, cut or
change the design before anything is written. Every decision below is a proposal; §11 collects the
ones that need an answer first.

**Scope of the first version (v1):**
- PostgreSQL only.
- Create, list, describe, rename, tag, start, stop, reboot and delete a database instance.
- A master user, a generated password shown once, and a connection endpoint inside a VPC.
- Console pages (Databases) and the API under `/api/v1/rds`.

**Not in v1** (each is a follow-up, not a precluded design):
- MySQL/MariaDB; read replicas; multi-host or high availability.
- Backups, snapshots and point-in-time recovery.
- Resize (instance class or storage), engine upgrades, parameter groups.
- Password reset on a running instance (needs the guest agent, §11).
- Public or cross-VPC access, security groups (still a global non-goal), metrics.

## 1. Shape

```
 console ──/api/v1/rds/*──▶ kumod ── service/dbinstance
                                      │
                                      ├─ store (SQLite: db_engines, db_instances; never a password)
                                      ├─ service/instance machinery (IP allocation, locks, tasks)
                                      └─ hypervisor.Hypervisor (interface)
                                           ├─ libvirt  (root disk + DATA disk + seed ISO + domain)
                                           └─ fake     (in memory; tests and KUMO_HYPERVISOR=fake)

 applications ──PostgreSQL protocol, TCP 5432──▶ the instance's private IP, inside the VPC
 kumod        ──TCP + protocol handshake probe──▶ the same address, to learn "available"
```

- **Layering is unchanged:** handlers call `internal/service/dbinstance`, which calls the store
  and the hypervisor. Handlers never call libvirt or SQL, services never import `gin`.
- **A database instance owns a compute instance.** `db_instances.instance_id` references an
  `instances` row, so the VM, its network interface, its IP and its libvirt domain are exactly what
  `04-libvirt-integration.md` already describes. The reconciler, the per-resource lock and the
  capacity accounting see an ordinary instance and need no special case.
- **Two sources of truth, as everywhere:** libvirt is the truth for whether the VM runs; SQLite is
  the truth for what Kumo adds (engine, version, master user, who created it, tags). Whether
  PostgreSQL itself is answering is observed by a probe (§7), never assumed from the VM state.
- **The VM is not editable as an instance.** The instance behind a database instance appears in
  the instance list marked as managed by `db-<id>`. Stop, start, reboot and terminate on it are
  refused with `409` and a message naming the database instance, because the lifecycle belongs to
  the service. Otherwise a terminate from the instance page would leave a database row pointing at
  nothing.
- **Long operations are tasks.** Create and delete boot or destroy a VM, so they are tasks
  (`dbinstance.create`, `dbinstance.delete`), as are start, stop and reboot. Rename and tag changes
  are one fast request and stay synchronous.

## 2. Configuration

The module needs no outside server, so it is **on by default**. It does need an *engine*: an image
that can run PostgreSQL (§3). With no engine registered, `POST /rds/db-instances` answers `409
conflict` with a message that says so, and the console shows the Databases pages with an empty
state that links to the engine setup.

| Variable | Default | Description |
|---|---|---|
| `KUMO_RDS` | `enabled` | `enabled` or `disabled`. Disabled: the `/api/v1/rds/*` routes answer `503 unavailable` and the console hides the Databases pages |
| `KUMO_RDS_PROBE_INTERVAL` | `10s` | how often a `creating` or `available` database instance is probed |
| `KUMO_RDS_CREATE_TIMEOUT` | `15m` | how long a new instance may take to start answering before it is marked `error` |
| `KUMO_RDS_DEFAULT_STORAGE_GB` | `20` | data disk size when the request omits it |

`/system/info` reports `rds: { enabled, engines }` so the console can gate the pages.

## 3. Engines

An **engine** ties a database engine and major version to an image that can run it.

- Engines are managed by administrators, like instance types (`01-api.md` §Instance types). A fresh
  node has none.
- An engine row names an **image** (`images.id`). Two kinds of image work:
  - **A stock cloud image** (Debian or Ubuntu LTS). Cloud-init installs the distribution's
    `postgresql` package on first boot. The VM therefore needs outbound access to the distribution
    mirror, which a NAT VPC has and an isolated one does not.
  - **A prepared image** with PostgreSQL already installed. First boot is faster and needs no
    network. This is the route for isolated VPCs and for air-gapped nodes.
- The version is **declared by the administrator** and recorded as the engine's major version. v1
  does not read it back from the guest; the version in the console is the one the engine row says.
  A mismatch is a configuration error the operator can see in `psql`.
- The first-boot script (§5) is written for Debian-family images (`apt`, the `postgresql` package,
  `pg_ctlcluster`). Red Hat family images are not supported in v1 and an engine pointing at one is
  rejected when it is registered, by checking the image's `os_family`/`default_user`.

## 4. Data model

Migration `0006_rds.sql`:

```sql
CREATE TABLE db_engines (
  id          TEXT PRIMARY KEY,                  -- postgres-16: <engine>-<major version>, chosen by the admin
  engine      TEXT NOT NULL CHECK (engine IN ('postgres')),
  version     TEXT NOT NULL,                     -- major version, e.g. '16'
  image_id    TEXT NOT NULL REFERENCES images(id),
  port        INTEGER NOT NULL DEFAULT 5432,
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE (engine, version)
);

CREATE TABLE db_instances (
  id              TEXT PRIMARY KEY,              -- db-<17 hex>
  name            TEXT NOT NULL,                 -- RFC-1123 label, unique among live rows
  state           TEXT NOT NULL CHECK (state IN
                   ('creating','available','stopping','stopped','starting','rebooting',
                    'deleting','error')),
  state_reason    TEXT,
  engine_id       TEXT NOT NULL REFERENCES db_engines(id),
  engine_version  TEXT NOT NULL,                 -- snapshot at create
  instance_type   TEXT NOT NULL REFERENCES instance_types(id),
  storage_gb      INTEGER NOT NULL,              -- data disk size, immutable in v1
  master_username TEXT NOT NULL,
  database_name   TEXT NOT NULL,                 -- the initial database
  instance_id     TEXT UNIQUE REFERENCES instances(id),  -- NULL until the VM row exists
  endpoint_ip     TEXT,                          -- snapshot of the VM's private IP
  port            INTEGER NOT NULL,
  created_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);
CREATE INDEX idx_db_instances_state ON db_instances(state);
CREATE UNIQUE INDEX idx_db_instances_name_live ON db_instances(name) WHERE state <> 'deleting';
```

- **No password column.** The master password is generated by kumod, returned once in the create
  response and never stored or logged. What reaches the VM is a SCRAM-SHA-256 verifier (§5), so the
  seed ISO, which stays attached to the domain, does not hold the password either.
- **`id` format:** `db-` + 17 hex from `internal/id` (new `PrefixDBInstance = "db"`), matching the
  other resources, so `HasResourceShape` validates path parameters.
- **Tags** live in `tags` under the resource id `db-<17 hex>`, as for instances.
- **Terminate deletes the row**, as for instances: it leaves nothing but its tasks behind.
- **Why `instance_id` can be `NULL`:** the `db_instances` row is inserted first so a failed create
  has something to mark `error`; the create task fills `instance_id` in once the VM row exists.
- **Immutable:** `engine_id`, `instance_type`, `storage_gb`, `master_username`, `database_name`,
  `instance_id`. Editable: `name` and tags.

## 5. Provisioning (task `dbinstance.create`)

```
validate (engine exists, instance type, storage 5–1024 GiB, name, subnet) → one txn:
  INSERT db_instance(state=creating) + the VM's instances row + eni + tasks
  1. Allocate IP and MAC (the existing subnet allocation)
  2. CreateRootDisk from the engine's image          (as instance.launch)
  3. CreateDataDisk  db-<id>-data.qcow2, storage_gb  (NEW, §6)
  4. CreateSeedISO   cloud-config from §5.1
  5. Static DHCP entry, domain XML with BOTH disks, DomainDefineXML, DomainCreate
  6. Wait for the domain to run; then probe the port until it answers (§7)
  7. UPDATE db_instance state=available, endpoint_ip; task succeeded
on failure at step N: undefine the domain, remove the DHCP entry, delete root disk, data disk and
seed ISO, state=error with the reason, as instance.launch does
```

It reuses the instance machinery rather than copying it, so the adoption and rollback rules of
`04-libvirt-integration.md` §3 apply unchanged: every step adopts what is already there, a
shutdown is not a failure, and the final write does not destroy a booted guest.

### 5.1 The cloud-config

Generated by a **pure function** (`internal/service/dbinstance/cloudinit.go`) from the engine, the
database name, the master user, the SCRAM verifier and the subnet CIDR, so it can be tested with
golden files and no hypervisor.

- Waits for the data disk by a stable name (the domain gives it the serial `kumo-data`, so the guest
  sees `/dev/disk/by-id/…kumo-data`), creates a filesystem on it only if it has none, mounts it at
  `/var/lib/postgresql` and records it in `/etc/fstab` with `nofail`.
- Installs `postgresql` if the image lacks it, initialises the cluster on the data disk, and
  creates the master role and the initial database.
- Sets the master role's password with **`ALTER ROLE … PASSWORD 'SCRAM-SHA-256$4096:…'`**. Postgres
  accepts a precomputed verifier, so kumod hashes the password with a random salt and ships only the
  hash. The verifier is not the password, but it is still a credential, so it is treated as one: it
  is not logged.
- `listen_addresses = '*'`, `password_encryption = scram-sha-256`, and a `pg_hba.conf` that allows
  `scram-sha-256` from the **subnet's CIDR** only. This is the substitute for security groups,
  which are a global non-goal. A client in another subnet of the same VPC is refused until
  security groups exist.
- TLS stays at the distribution default (a self-signed certificate), so connections can encrypt
  but clients are not given a certificate to verify. §10 says so plainly.
- Everything runs as a boot-time script, so a requeued create that adopts an existing domain does
  not run it twice: cloud-init's per-instance semantics already guarantee that.

## 6. Hypervisor changes

The only change to `internal/hypervisor` is a **second disk** on a domain. This is why a database
instance can't be built on today's `Hypervisor` unchanged.

- `CreateDataDisk(ctx, name, sizeGiB)` creates an empty qcow2 volume in the `kumo-instances` pool.
  Volume names come from `hypervisor.DataDiskName(id)` (`db-<id>-data.qcow2`), next to
  `RootDiskName` and `SeedISOName`, so the creator, the libvirt backend and the fake cannot
  disagree on a name a rollback deletes.
- The domain spec gains an optional list of extra disks. The XML adds a `virtio` disk with serial
  `kumo-data`; the existing root and seed disks are untouched.
- The fake records data disks so rollback and terminate are testable without libvirt.
- A volume is still **not a first-class resource** (`00-architecture.md` §1 non-goals). The data disk
  belongs to the database instance and is deleted with it. Nothing can detach or move it.

## 7. Readiness, lifecycle and the reconciler

- **`available` means the port answers.** After the VM is running, the service probes the endpoint
  with a TCP connect followed by a PostgreSQL `SSLRequest` handshake: a server that answers `S` or
  `N` is a PostgreSQL server accepting connections. No driver and no credentials are needed, so
  **no new dependency** is added. A probe that fails keeps the instance `creating` until
  `KUMO_RDS_CREATE_TIMEOUT`, then `error` with the last probe error as the reason.
- **State follows the VM.** `stopped`, `starting` and `rebooting` come from the lifecycle tasks.
  A start or reboot returns to `available` only when the probe answers again.
- **Reconciler:** the existing 10-second pass already marks a VM whose domain disappeared as
  `error`. A database instance whose VM is `error` follows. An `available` instance whose probe
  fails three times in a row is shown as `error` with `probe_failed`, and returns to `available`
  by itself when the probe succeeds. Nothing is restarted automatically.
- **One lock per instance:** a database operation takes the VM's `instance.LockKey` (§3 of the
  libvirt doc), so a database stop and a stray lifecycle request on the VM cannot interleave.
- **Stop is the VM's graceful ACPI shutdown**, which lets systemd stop PostgreSQL cleanly. `force`
  is available and is documented as risking crash recovery on the next start.
- **Delete is destructive and immediate:** the domain, the root disk, the data disk and the seed ISO
  are removed, and the data is gone. There is no final snapshot in v1. The console says so in the
  confirmation, and requires typing the database instance's name.

## 8. API (`/api/v1/rds`)

Conventions are those of `01-api.md`: JSON, the error envelope, `202 Accepted` with a `Location`
for tasks, page tokens on lists. `viewer` reads, `admin` writes.

| Method | Path | Description |
|---|---|---|
| GET | `/rds/engines` | List engines |
| POST | `/rds/engines` | (admin) Register an engine `{engine, version, image_id, port?, description?}` |
| DELETE | `/rds/engines/{id}` | (admin) `409` while any database instance uses it |
| GET | `/rds/db-instances` | List (`?state=available&q=orders`, `?page_size=`, `?sort=created_at:desc`) |
| POST | `/rds/db-instances` | Create (async, `202`). The only response that carries the password |
| GET | `/rds/db-instances/{id}` | Detail, with the connection block below |
| PATCH | `/rds/db-instances/{id}` | Update `name`, `tags` |
| POST | `/rds/db-instances/{id}/start` | `202` if `stopped` |
| POST | `/rds/db-instances/{id}/stop` | `202`; `?force=true` destroys the domain |
| POST | `/rds/db-instances/{id}/reboot` | `202` if `available` |
| DELETE | `/rds/db-instances/{id}` | `202`; destroys the data |

Create request:
```json
{ "name": "orders-db", "engine_id": "postgres-16", "instance_type": "kumo.small",
  "storage_gb": 20, "master_username": "app", "database_name": "orders",
  "subnet_id": "subnet-…", "tags": {"env": "dev"} }
```
Validation: `name` 1–63 chars, RFC-1123 label, unique among live rows; `master_username`
1–32 chars matching `[a-z_][a-z0-9_]*` and not `postgres` or starting with `pg_`;
`database_name` the same rule; `storage_gb` 5–1024; `subnet_id` optional, defaulting as for
instances; a missing or unknown `engine_id` is `400`/`404`; capacity is checked as for a launch
and failure is `422 quota_exceeded`. A request that supplies its own `master_password` is
rejected in v1 so that the only password that exists is one that was shown to the caller.

Create response (`202`):
```json
{ "id": "db-0123456789abcdef0", "name": "orders-db", "state": "creating",
  "engine": "postgres", "engine_version": "16", "instance_type": "kumo.small",
  "storage_gb": 20, "master_username": "app", "database_name": "orders",
  "master_password": "…shown once…", "tags": {"env": "dev"} }
```

Detail carries a `connection` block once the address is known:
```json
{ "connection": { "host": "10.0.1.14", "port": 5432, "database": "orders",
                  "username": "app", "sslmode": "require",
                  "uri": "postgresql://app@10.0.1.14:5432/orders?sslmode=require" } }
```
The URI never contains a password.

## 9. Console

A **Databases** group in the side navigation, hidden when `rds.enabled` is false, as the Storage
group is.

| Path | Page | Notes |
|---|---|---|
| `/databases` | DatabasesList | Table with `useCollection`, state `StatusIndicator`, actions |
| `/databases/create` | CreateDatabase | Engine, name, class, storage, subnet, master user, database, tags |
| `/databases/:id` | DatabaseDetail | Tabs: Connectivity, Configuration, Tags, Activity (tasks) |
| `/database-engines` | EnginesList | (admin) register and remove engines |

- **The password** is shown once in a modal after create, with Copy, and cannot be fetched again.
  The modal says that a lost password cannot be reset in this version.
- **State mapping** for `StatusIndicator`: `creating`/`starting`/`rebooting`/`stopping`/`deleting` →
  in-progress, `available` → success, `stopped` → stopped, `error` → error with `state_reason`.
- **Connectivity** shows host, port and the URI, plus a copyable `psql` command with
  `-W` so the password is not on a command line.
- **Delete** is a confirmation `Modal` that names the data loss and requires typing the name.
- Standard Cloudscape rules from `03-frontend.md` apply: RTK Query in `features/databases/api.ts`,
  `Flashbar` for errors, inline `FormField` errors from the API `details`.

## 10. Security

- **The master password** is generated with `crypto/rand`, at least 24 characters, returned once,
  never stored, never logged, and absent from `GET`.
- **Only a SCRAM verifier** reaches the VM (§5.1). It still sits in the seed ISO, so it is as
  sensitive as `user_data` is for an instance; the documentation says so in the same words.
- **Network exposure is the subnet's CIDR**, by `pg_hba.conf`. There is no public endpoint in v1 and
  the module must not add a port forward on the host.
- **TLS is not verified.** The certificate is the distribution's self-signed one, so `sslmode=require`
  encrypts but does not authenticate the server. Certificates issued by Kumo are a follow-up.
- **Input is validated and bounded** at the API boundary: the name, user and database rules of §8
  are what stop a value being interpolated into the cloud-config or the SQL it generates. The
  generated cloud-config places each value in a fixed position and **quotes it through a single
  helper**, never by string concatenation, and the SQL identifiers are quoted with PostgreSQL's
  identifier rules. A test feeds the helper hostile names.
- **Logs** carry the database instance id, the engine and the task id. They do not carry the
  password, the verifier or the generated cloud-config.
- All routes require the JWT of `/api/v1/*`.

## 11. Open questions (answer before approval)

1. **Provisioning model.** A VM per database (proposed here) is the heaviest option and the one
   the existing compute code supports best. A container on the host would be lighter but adds a
   runtime dependency, which `AGENTS.md` rule 6 forbids for Kumo itself. Is a VM acceptable?
2. **Stock image or prepared image first?** Installing from a mirror makes a first create take
   minutes and need outbound access. Is shipping a documented prepared-image recipe in scope, or is
   stock-image-only enough for v1?
3. **Password reset.** Without the QEMU guest agent there is no way to change a running instance's
   password. Adding the guest agent to the domain (a `virtio-serial` channel) is a small change to
   the domain XML, and would unlock reset, graceful readiness and snapshots. In v1 or later?
4. **Backups.** The cheapest useful backup is a `pg_dump` pushed to a bucket in Kumo's own object
   storage. Is that the first follow-up, or is volume snapshotting preferred?
5. **Engines.** PostgreSQL only is proposed. Should MySQL be designed for now (the `engine` check
   constraint and cloud-config generator are the only places it would touch)?
6. **Security groups substitute.** `pg_hba.conf` limited to the subnet is proposed. Is that
   acceptable, or should the whole VPC be allowed?

## 12. Milestone M7 — proposed tasks

Each task is one reviewable change, in order, with its acceptance criteria. This is the shape the
roadmap uses; it is **not** added to `05-roadmap.md` until the design is approved.

- [ ] **M7.1 Data model.** `PrefixDBInstance` in `internal/id`, migration `0006_rds.sql`, sqlc queries
  for `db_engines` and `db_instances`, store tests.
  AC: migration applies on an empty DB and on a populated one; `make sqlc` is a clean diff;
  `HasResourceShape` accepts `db-…`; a live-name uniqueness test passes.
- [ ] **M7.2 Second disk in the hypervisor.** `CreateDataDisk`, `DataDiskName`, the optional extra
  disk on the domain spec, libvirt XML, fake implementation.
  AC: fake tests for create, adopt-on-retry and delete; a golden XML test for a domain with a data
  disk and the existing golden tests unchanged.
- [ ] **M7.3 Cloud-config generator.** The pure function, the SCRAM-SHA-256 verifier, the quoting
  helper.
  AC: golden-file tests; a verifier test against a known vector; hostile-input tests prove no
  value escapes its position; the password never appears in the output.
- [ ] **M7.4 Service layer.** `internal/service/dbinstance`: create, delete, start, stop, reboot,
  rename and tag, on the fake hypervisor, with a probe interface and its fake.
  AC: table-driven tests for every transition and every failure step leaving no orphan volume or
  domain; a lifecycle request against the managed VM is refused with `409`; the probe timeout
  path produces `error`.
- [ ] **M7.5 Readiness probe and reconciler hook.** The `SSLRequest` probe, the three-failure rule.
  AC: tests against a local TCP listener that speaks the handshake, one that accepts and says
  nothing, and one that refuses.
- [ ] **M7.6 API.** Handlers, the error mapping, `api/openapi.yaml`, `01-api.md`, the authorization
  matrix.
  AC: `httptest` coverage of every route and status; `viewer` gets `403` on writes; the password
  appears in the create response only.
- [ ] **M7.7 Engines.** `KUMO_RDS*` config, engine registration with image validation, `/system/info`.
  AC: registering an engine on a non-Debian image is refused; deleting an engine in use is `409`;
  the module disabled answers `503`.
- [ ] **M7.8 Console.** The Databases pages and the engines page, generated API types.
  AC: component tests for the list, create form and the one-time password modal; the pages are
  hidden when the module is off.
- [ ] **M7.9 End to end and docs.** Playwright on the fake hypervisor, `docs/install.md` section,
  README row to ✅.
  AC: a create-to-delete run in the browser; the install guide covers registering an engine.
