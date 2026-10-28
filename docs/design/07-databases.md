# 07 — Databases (managed Postgres)

Kumo's second service beyond compute. A **DB instance** is a Postgres server the
console creates, starts, stops and deletes; the virtual machine underneath it is
Kumo's, not the user's, and does not appear on the Instances pages. This is the
"modules registered under `/api/v1/<service>`" extension point of
`00-architecture.md` §10, the same shape object storage uses in
`06-object-storage.md`.

Unlike object storage, this module needs **no server Kumo does not ship**. It
needs one thing an operator must supply: a **golden image** with Postgres already
installed, imported like any other image. Without it, the Databases pages explain
how to get one and nothing else works.

## 0. Status

**Designed, not built.** Proposed as milestone M7 (`05-roadmap.md`), after the
open M5 and M6 items unless explicitly approved ahead of them.

### Scope of the first version

Deliberately narrow. The point is one engine booting fast and correctly, not a
feature matrix.

| In v1 | Not in v1 |
|---|---|
| Postgres 18, x86_64 | MySQL, other Postgres majors, aarch64 |
| Create, list, detail, stop, start, reboot, delete | Modify (compute, storage, password, parameters), minor-version upgrade |
| Admin role + initial database at create time | Extra roles and databases, extension management |
| Reachable from inside its VPC | Public access, port forwarding, TLS |
| Plaintext connections inside the VPC | Backups, snapshots, restore, PITR |
| One server per DB instance | Read replicas, HA, failover, Multi-AZ |
| Separate data disk per instance | Storage growth, parameter groups, tags, metrics, slow-query logs |

Every row on the right is additive: §9 records which of them the schema and the
image recipe already leave room for, and which would need a redesign.

### Decisions this version fixes

| # | Decision | Rationale |
|---|---|---|
| R1 | **Postgres 18 only** | One engine, one major. The engine and version are columns, not assumptions, so a second one is data plus an image |
| R2 | **Golden image built once, out of band, imported from a URL** | Reuses the existing `image.import` task whole. No image-building tooling on the Kumo host, so `CGO_ENABLED=0`, no-`exec.Command` and single-binary all stay true |
| R3 | **No in-guest agent** | Nothing to build, ship, version or authenticate. The cost is that every operation beyond the lifecycle in §4 is out of scope until there is one |
| R4 | **The data directory is a separate qcow2 disk** | The only v1 choice that keeps storage growth and swapping the root disk for a patched image possible later without dump/restore |
| R5 | **No TLS between client and server** | Plaintext inside a VPC, like the object storage module before the M5 TLS task. Stated in the console, not hidden |
| R6 | **VPC-internal only** | Security groups are an explicit non-goal (`00-architecture.md` §1), so there is no mechanism that could make a database safe to expose. The engine binds to the VPC address and `pg_hba.conf` admits the VPC CIDR and nothing else |
| R7 | **The backing VM is a row in `instances`, hidden from the Instances pages** | Capacity accounting, the reconciler, IP/MAC allocation, orphan reporting and launch rollback all keep working unchanged. A separate table would mean reimplementing each of them |
| R8 | **Kumo never holds the admin password**, in any form, anywhere | It computes a SCRAM-SHA-256 verifier from it in the request and discards the password. See §5 |
| R9 | **The endpoint is a DNS name, not an address** | dnsmasq already serves names for the static DHCP hosts Kumo writes, so a stable name costs one field |

## 1. Shape

```
 console ──/api/v1/rds/*──▶ kumod ── service/dbinstance
                                      │
                                      ├─ store (SQLite: db_instances + a hidden row in instances)
                                      ├─ service/instance (launch / stop / start / terminate the VM)
                                      └─ hypervisor.Hypervisor (data disk, domain, DHCP host)

 apps in the VPC ──TCP 5432──▶ the guest's private IP        bytes never pass through kumod
                               <identifier>.<vpc-name>
```

- **Layering is unchanged.** Handlers call `service/dbinstance`, which calls the
  store, `service/instance` and the hypervisor. Nothing in the database service
  touches libvirt directly and nothing imports `gin`.
- **Two sources of truth, as everywhere else.** libvirt owns whether the guest is
  running; SQLite owns the DB instance's metadata, its identifier, its engine and
  which instance backs it. A third fact — whether Postgres itself is accepting
  connections — belongs to neither, and §6 says how it is learned.
- **One long operation, one task.** `db.create` and `db.delete` are tasks, for the
  same reason `instance.launch` is. Stop, start and reboot reuse the instance
  lifecycle tasks.

## 2. Configuration

| Variable | Default | Description |
|---|---|---|
| `KUMO_DATABASES` | `on` | `on` or `off`. With `off`, `/api/v1/rds/*` answers `503 unavailable` and the console hides the Databases pages |
| `KUMO_DB_PROBE_INTERVAL` | `10s` | How often the readiness probe of §6 checks a creating or running DB instance |
| `KUMO_DB_READY_TIMEOUT` | `10m` | How long a `db.create` task waits for the engine before the DB instance is `failed` |

The module is **on by default**, unlike object storage: it adds no dependency on
anything outside the binary, so there is nothing for an operator to turn on. What
it does need is an engine image, and that is a state the console reports rather
than a configuration error:

- `/system/info` gains `databases: {enabled, engines: [{engine, version}]}`, one
  entry per engine and version that is `available`. The image id is left out:
  that payload is served without a token, the same reason the object store's
  endpoint is left out of it, and nothing needs one, because a create request
  names an engine and a version rather than an image.
- An empty `engines` list is not an error. The Databases pages show how to import
  the image, and `POST /rds/db-instances` answers `409 conflict` with
  `no_engine_image`.
- `kumod doctor` **warns** when the module is on and no engine image is available,
  and says nothing when it is off.

## 3. Data model

### Migrations

The schema arrives in two migrations, because the engine images (M7.2) and the
DB instances themselves (M7.3) are separate tasks and an applied migration is
never edited: `0006_engine_images.sql` adds the three columns to `images`, and
`0007_db_instances.sql` adds `instances.managed_by` and the `db_instances`
table.

### `0006_engine_images.sql` and `0007_db_instances.sql`

```sql
-- +goose Up

-- The VM behind a DB instance is an ordinary instance that the user does not own.
-- '' is a user's own instance; 'rds' is one this module created. Every list and
-- search query filters on it, so a managed VM cannot appear on the Instances
-- pages, while capacity accounting, the reconciler and the orphan report go on
-- counting it exactly as before.
ALTER TABLE instances ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_instances_managed ON instances(managed_by);

-- An engine image is an ordinary image that the user does not launch. 'user' is
-- an image the Images pages offer; 'engine' is a golden image this module boots.
-- engine/engine_version are set on an engine image and NULL otherwise, and are
-- how a create request resolves "Postgres 18" to a volume.
ALTER TABLE images ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'
  CHECK (kind IN ('user','engine'));
ALTER TABLE images ADD COLUMN engine TEXT;
ALTER TABLE images ADD COLUMN engine_version TEXT;
CREATE INDEX idx_images_engine ON images(kind, engine, engine_version);

CREATE TABLE db_instances (
  id             TEXT PRIMARY KEY,               -- db-<17 hex>
  identifier     TEXT NOT NULL,                  -- user-chosen, immutable; seeds the endpoint name
  state          TEXT NOT NULL CHECK (state IN
                  ('creating','available','starting','stopping','stopped',
                   'rebooting','deleting','failed')),
  state_reason   TEXT,
  engine         TEXT NOT NULL CHECK (engine IN ('postgres')),
  engine_version TEXT NOT NULL,                  -- '18'
  image_id       TEXT NOT NULL REFERENCES images(id),
  instance_id    TEXT NOT NULL REFERENCES instances(id), -- the hidden VM; ON DELETE is manual, see §4
  instance_type  TEXT NOT NULL REFERENCES instance_types(id),
  storage_gb     INTEGER NOT NULL,               -- the data disk, not the root disk
  port           INTEGER NOT NULL DEFAULT 5432,
  admin_username TEXT NOT NULL,                  -- the role the create request named
  database_name  TEXT NOT NULL,                  -- the database created and owned by it
  endpoint       TEXT,                           -- <identifier>.<vpc name>, written once the guest has an address
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
-- One live identifier at a time, the live-name rule of `instances` applied here.
CREATE UNIQUE INDEX idx_db_instances_identifier ON db_instances(identifier);
CREATE UNIQUE INDEX idx_db_instances_instance ON db_instances(instance_id);

-- +goose Down
-- (drop in reverse order; SQLite cannot drop the added columns before 3.35)
```

Notes on what is **not** in the table:

- **No password column, and no password hash column.** R8. The verifier kumod
  computes is written into the seed data and then dropped; nothing persists it.
  A `db_instances` row therefore cannot be used to authenticate to its own
  database, which is the point.
- **No `vcpus`/`memory_mib` snapshot.** The backing `instances` row already
  snapshots them at launch, and that row is the one capacity sums.
- **`instance_id` is `NOT NULL` with no cascade.** A DB instance without its VM is
  not a state worth representing; §4 deletes the two rows in one transaction, in
  the right order.
- **`identifier` is immutable**, because it is the endpoint. Renaming it would
  silently break every client.

### Reserved names and validation

- `identifier`: 1–60 chars, `[a-z][a-z0-9-]*`, no trailing or doubled `-`. It
  becomes a DNS label and the backing instance's name, so it must also not
  collide with a live instance name — enforced by `idx_instances_name_live`,
  which the backing row shares.
- `admin_username`: 1–63 chars, `[a-z_][a-z0-9_]*`, and **not** one of
  `postgres`, `pg_*` (every `pg_` prefix is reserved by Postgres itself),
  `template0`, `template1`, `rdsadmin`, `kumo`.
- `database_name`: same shape as `admin_username`, not `postgres`, `template0`,
  `template1`.
- `storage_gb`: 5–2048, and at least the engine image's `min_disk_gb`.
- The admin password: 10–128 chars, the same floor as a Kumo user's password
  (`01-api.md` §Auth). Printable ASCII only, so that nothing downstream has to
  think about normalization.

## 4. Lifecycle

### Create (task `db.create`)

The handler validates, resolves and reserves synchronously, then returns `202`
with the DB instance `creating`. It never waits for a boot.

```
validate (identifier, username, database, storage, password)
resolve   the engine image: kind='engine', engine, engine_version, state='available'
          → 409 no_engine_image when there is none
compute   the SCRAM-SHA-256 verifier from the password; discard the password (§5)
reserve   in 1 txn, the same transaction service/instance already uses for a launch:
            capacity check (vCPU, memory, root disk + storage_gb)
            INSERT instances(..., managed_by='rds', user_data=NULL, key_pair_id=NULL)
            INSERT network_interfaces (lowest free IP in the subnet, 52:54:00: MAC)
            INSERT db_instances(state='creating', instance_id=…)
            INSERT task(db.create)
  1. CreateDataDisk: db-<id>-data.qcow2 in kumo-instances, capacity = storage_gb GiB,
     no backing file
  2. CreateRootDisk: the usual overlay on the engine image volume
  3. CreateSeedISO: the generated cloud-config of §5, carrying the verifier
  4. AddDHCPHost: <host mac ip name='<identifier>'/> — the name, not the instance id,
     is what makes the endpoint resolve
  5. DefineAndStart with both disks: the root overlay, and the data disk carrying
     `<serial>kumodata</serial>` so the guest can find it by id rather than by
     enumeration order (§5 step 1)
  6. Wait for the domain to run, then for the engine: poll the readiness probe of §6
     every KUMO_DB_PROBE_INTERVAL up to KUMO_DB_READY_TIMEOUT
  7. In 1 txn: instances → running, db_instances → available, endpoint written
on failure at step N: the instance rollback of 04-libvirt-integration.md §3, plus the
  data disk, and db_instances → failed with the reason
```

Everything the launch flow already guarantees carries over unchanged, and must
keep being true here: **every step adopts what is already there** (a requeued
task finds its own volumes and its own domain), **a shutdown is not a failure**
(the rows stay `creating` and nothing is deleted), and **the final write is not a
reason to destroy a running guest**. A `db.create` worker that cannot write step 7
leaves a healthy database alone and lets the reconciler catch up.

Two failures are worth naming because they are not instance failures:

- **The domain runs and the engine never answers.** The VM is fine, so there is
  nothing to roll back and throwing it away destroys the evidence. The DB instance
  goes `failed` with a reason, the VM stays defined and stopped-or-running as it
  is, and delete is the only way out. The guest's serial log is where the operator
  looks, and it is already captured at
  `$KUMO_DATA_DIR/console/<instance-id>.log`.
- **The data disk already holds a cluster.** Only reachable through a requeued
  task, and it is the normal case for one: the first-boot script of §5 is
  idempotent and leaves an initialized disk alone, so step 6 simply finds the
  engine up.

### Stop, start, reboot

Delegated to `service/instance` against the hidden row, with the DB instance state
following: `stopping`/`stopped`, `starting`/`available`, `rebooting`/`available`.

- **Stop is an ACPI shutdown**, never `Destroy`, so Postgres gets its shutdown
  checkpoint. The timeout before a hard stop is the instance service's existing
  one.
- **Start re-runs the readiness probe** before reporting `available`; the IP and
  MAC are stable across stop/start by construction (§6.4 of
  `04-libvirt-integration.md`), so the endpoint does not move.
- There is no state in which a user's request reaches the backing instance
  directly. `POST /instances/{id}/stop|start|reboot` and `DELETE /instances/{id}`
  answer **`409 conflict`** for a row with `managed_by <> ''`, with a message
  naming the DB instance and its id. A viewer gets `403` first, as everywhere.

### Delete (task `db.delete`)

Requires the identifier typed to confirm, in the console (`AGENTS.md`:
destructive actions take a confirmation `Modal`).

```
db_instances → deleting; queue db.delete
  1. the instance terminate flow of 04-libvirt-integration.md §3:
     Destroy → Undefine → remove the DHCP host → delete -root.qcow2 and -seed.iso
  2. delete db-<id>-data.qcow2
  3. in 1 txn: DELETE db_instances, then DELETE instances (the interfaces follow
     through the foreign key cascade)
  4. remove the console log
```

Idempotent step by step, like terminate. **The data is gone**: there is no final
snapshot and no recovery, which is what the confirmation says in as many words.

`Undefine` removes every volume the domain declares, which now includes the data
disk — so step 2 is the pass that cleans up when there was no domain left to read
the disks from, exactly as the `-root.qcow2` pass is today.

## 5. The admin credential

This is the part a golden image and no agent make interesting. The password
arrives in an HTTPS request and has to reach a Postgres role inside a guest that
Kumo cannot talk to. The only channel is the cloud-init seed, and a seed ISO is a
file in the instances pool.

**So the password never goes into the seed.** Postgres accepts a pre-computed
SCRAM-SHA-256 verifier wherever it accepts a password:

```sql
CREATE ROLE app LOGIN PASSWORD 'SCRAM-SHA-256$4096:<b64 salt>$<b64 StoredKey>:<b64 ServerKey>';
```

kumod computes it in `internal/service/dbcred`, in pure Go with the standard
library:

```
SaltedPassword = PBKDF2-HMAC-SHA256(password, salt, 4096, 32)   // crypto/pbkdf2
ClientKey      = HMAC-SHA256(SaltedPassword, "Client Key")
StoredKey      = SHA256(ClientKey)
ServerKey      = HMAC-SHA256(SaltedPassword, "Server Key")
verifier       = "SCRAM-SHA-256$4096:" + b64(salt) + "$" + b64(StoredKey) + ":" + b64(ServerKey)
```

with a fresh 16-byte `crypto/rand` salt per DB instance. The exact string format
is load-bearing and must be **verified against a real Postgres 18** in the M7
task, not assumed from this document.

What this buys, and what it does not:

- The cleartext password exists only in the request body and in one local variable.
  It is not stored, not logged, not written to the task payload, and not placed in
  the seed.
- The verifier **is** in the seed ISO, and a verifier is a password hash: an
  offline guessing target at 4096 PBKDF2 iterations, and enough to impersonate the
  server to a client, but not the password. The ISO is in `kumo-instances`, owned
  by the pool's QEMU user and not world-readable, and the first-boot script
  shreds the guest's copy under `/var/lib/cloud`.
- Because nothing is stored, **there is no password reset** and no "show password
  again". Both need the agent of R3. The console says so at create time, next to
  the password field.
- `user_data` for a managed instance is `NULL` in the database (§3), so the
  generated cloud-config is not persisted anywhere either. The existing rule —
  never log `user_data` — covers the rest.

### Generated cloud-config

Built by `internal/service/dbcloudinit`, unit-tested on the rendered output, the
way `internal/service/cloudinit` is. No key pair is attached and
`ssh_pwauth: false`, so the guest has no interactive login at all: a DB instance
is not a machine the user has shell on.

The script's shape, with the ordering that makes §6 sound:

```
1. if /dev/disk/by-id/virtio-kumodata has no filesystem: mkfs.ext4 -L pgdata on it
2. mount LABEL=pgdata /var/lib/postgresql, and add the fstab entry by label
3. if there is no cluster on it:
     pg_createcluster 18 main --datadir=/var/lib/postgresql/18/main \
       -- --encoding=UTF8 --locale=C.UTF-8 --auth-local=peer --auth-host=scram-sha-256
   (the image ships with no cluster: §7 drops the packaging's own)
4. write listen_addresses='localhost', port, password_encryption=scram-sha-256,
   ssl=off, and the memory settings of §7
5. write pg_hba.conf: local peer for postgres, host scram-sha-256 from the VPC CIDR only
6. start the cluster on localhost, then:
     CREATE ROLE <admin> LOGIN CREATEDB PASSWORD '<verifier>';
     CREATE DATABASE <database> OWNER <admin>;
     REVOKE ALL ON DATABASE <database> FROM PUBLIC;
7. set listen_addresses='*' and restart the cluster
8. shred the seed copies under /var/lib/cloud
```

**Step 1 finds the disk by serial, not by name.** `/dev/vdb` is whichever disk
libvirt attached second, and a first boot that formats the wrong one is not a
failure worth debugging twice — hence the `<serial>` in §4 step 5.

**Step 7 is last on purpose.** Postgres does not accept a connection on the VPC
address until the role and the database exist, which is what lets a bare TCP
connect be a truthful readiness signal in §6 without a Postgres driver anywhere
in the binary.

Every value interpolated into that script is validated first (§3) and quoted;
the verifier is base64 and `$`-delimited, so it is single-quoted in SQL and
passed to `psql` through a heredoc rather than an argument.

## 6. Readiness and reconciliation

**The probe.** A TCP connect to the guest's private IP on `port`, with a short
dial timeout, from kumod. Nothing is sent and nothing is parsed — no Postgres
driver is added to the binary (`AGENTS.md`: ask before adding a dependency, and
there is no need to here).

The probe means "the engine is listening on the VPC address", and because of step
7 of §5 that implies setup finished. It does **not** mean a client can log in: a
wrong password is indistinguishable from a right one, by design.

**Who probes.** The `db.create` task, while it waits (step 6), and the existing
reconciler loop, for every DB instance whose backing instance is `running`. The
reconciler's job here is one transition in each direction:

| Observed | DB instance state | Action |
|---|---|---|
| domain running, probe succeeds | `available` | none |
| domain running, probe fails | `available` | → `failed`, reason "engine not responding" |
| domain running, probe succeeds | `failed` | → `available`, reason cleared |
| domain shutoff | `available` | → `stopped` (the VM was stopped out of band) |
| domain missing | any | the existing orphan/`error` handling of the instance row; DB instance → `failed` |

A probe failure is never a reason to touch the guest. The reconciler reports; the
operator or a reboot acts.

**Rate.** One probe per DB instance per `KUMO_DB_PROBE_INTERVAL`, in the pass that
already walks the instances, so the cost is one dial per database per tick and a
node with no databases does no work.

## 7. The golden image

Built **once, outside Kumo**, published as a qcow2 with a SHA256, and imported
through `POST /images/import` with `kind=engine`, `engine=postgres`,
`engine_version=18`. The curated `/images/catalog` gains the entry so the console
offers a one-click import. Admin only, like every image import.

The recipe lives in `packaging/images/postgres-18/` as a script plus a
cloud-config, runnable on any machine with libvirt or qemu — **not** on the Kumo
host as part of a release, and nothing in `kumod` builds or depends on it.

Base: the Ubuntu 24.04 LTS cloud image, plus the PGDG apt repository for
`postgresql-18`.

What the recipe must do, beyond installing the engine:

- **Drop the packaging's cluster.** `postgresql-18` runs `initdb` on install, in
  `/var/lib/postgresql/18/main` on the root disk. `pg_dropcluster 18 main --stop`
  removes it, so first boot creates the cluster on the data disk instead and the
  image carries no cluster identity at all.
- **Leave `postgresql.service` enabled but harmless**, with no cluster to start.
  cloud-init creates the cluster before anything needs it.
- **Pre-install what first boot needs** so no boot ever reaches the network:
  `e2fsprogs`, and nothing else beyond the engine and cloud-init.
- **Strip the clone-unsafe identity**: truncate `/etc/machine-id`, delete the SSH
  host keys (`ssh-keygen` regenerates them per boot), `cloud-init clean --logs`,
  clear `/var/log` and the shell history, remove the apt lists.
- **No credentials of any kind**, no authorized keys, no default password, and
  `PasswordAuthentication no`.
- **Record the memory settings as a template**, not as values: first boot writes
  `shared_buffers` at 25% and `effective_cache_size` at 60% of the instance type's
  memory, which cloud-init knows and the image cannot.
- **`fstrim` the free space and flatten** with `qemu-img convert -O qcow2`, and
  record the SHA256 of exactly the published file. **Uncompressed**: the file is
  the read-only backing store of every DB instance's root overlay, so `-c` would
  trade a smaller download for decompressing every cold read a database makes
  for as long as the instance lives.

The published image is immutable. A patched Postgres 18 is a **new image** with a
new version string in its name, imported alongside the old one; existing DB
instances keep the backing file they booted from, and `resolve the engine image`
in §4 picks the newest `available` one for new DB instances only.

## 8. API (`/api/v1/rds`)

Full schemas go in `api/openapi.yaml` and the `01-api.md` section, in the PR that
implements each endpoint.

| Method | Path | Description |
|---|---|---|
| GET | `/rds/db-instances` | List, paged and sorted like every collection; `?q=` on the identifier |
| POST | `/rds/db-instances` | (admin) Create → `202` with the DB instance `creating` and `Location: /api/v1/tasks/<id>` |
| GET | `/rds/db-instances/{id}` | Detail, with the endpoint, the port, the admin username and the database name |
| POST | `/rds/db-instances/{id}/stop` | (admin) → `202`. `409` unless `available` |
| POST | `/rds/db-instances/{id}/start` | (admin) → `202`. `409` unless `stopped` |
| POST | `/rds/db-instances/{id}/reboot` | (admin) → `202`. `409` unless `available` |
| DELETE | `/rds/db-instances/{id}` | (admin) → `202`, state `deleting`. Allowed from every state except `deleting` |

Create body:

```json
{ "identifier": "orders", "engine": "postgres", "engine_version": "18",
  "instance_type": "kumo.medium", "storage_gb": 20, "subnet_id": "subnet-…",
  "admin_username": "app", "admin_password": "…", "database_name": "orders" }
```

`engine_version` may be omitted and defaults to the newest available. `subnet_id`
may be omitted and defaults the way a launch does. `port` is not accepted in v1.

A DB instance in a response never carries the password, the verifier, the salt,
or the backing instance's `user_data`. It does carry `instance_id`: an operator
debugging a stuck database needs the console log and the orphan report to make
sense, and hiding the id makes the module harder to support without making it
safer.

Error codes beyond the standard set: `no_engine_image` (`409`) when the module is
on and no engine image is `available`.

## 9. What this leaves room for

Worth recording, because it is the difference between "not in v1" and "would need
a redesign".

**Cheap later, because of a decision above:**
- **A second engine or major version** — R1: both are columns, and MySQL is an
  image, a `dbcloudinit` renderer and a port.
- **Storage growth** — R4: `qemu-img resize` on `db-<id>-data.qcow2` plus a
  `resize2fs` the guest must run, which is the one piece needing a reboot or an
  agent.
- **A patched engine image** — R4 and §7: the data lives on its own disk, so
  swapping the root overlay for one backed by the new image is a stop, a redefine
  and a start.
- **Compute resize** — the backing row is an ordinary instance, so whatever
  instance resize lands applies, plus re-rendering the memory settings.
- **Tags** — the generic `tags` table keyed on `db-<id>`, as buckets use
  `bucket/<name>`.
- **Public access** — needs security groups first, which is the real blocker, not
  anything here.

**Needs the agent of R3:**
- Password reset and rotation, extra roles and databases, parameter changes
  without a reboot, logical backups to an M6 bucket, engine and query metrics, and
  a readiness signal stronger than "the port is open".

The seed ISO should therefore carry a per-DB-instance random token and the kumod
callback URL from the start, even though nothing reads them in v1, so that adding
an agent does not mean rebuilding and re-publishing every image.

**Would need a redesign:**
- **HA and failover** — one DB instance is one VM by construction here, and
  nothing in the schema expresses a pair.
- **Point-in-time recovery** — needs WAL archiving to somewhere durable from the
  first boot, which means deciding the archive target before the first database is
  created, not after.
