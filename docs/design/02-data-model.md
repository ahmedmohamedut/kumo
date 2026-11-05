# 02 — Data Model (SQLite)

Migrations: `internal/store/migrations/NNNN_name.sql` (goose, embedded). Queries: `internal/store/queries/*.sql` (sqlc, engine `sqlite`).
Pragmas on every connection: `journal_mode=WAL; foreign_keys=ON; busy_timeout=5000; synchronous=NORMAL`.
Use a single writer connection (`SetMaxOpenConns(1)` for writes) or a short retry on `SQLITE_BUSY`.

## Initial schema (0001_init.sql)

```sql
-- +goose Up
CREATE TABLE users (
  id            TEXT PRIMARY KEY,                -- UUIDv7
  username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  password_hash TEXT NOT NULL,                   -- argon2id PHC string
  role          TEXT NOT NULL CHECK (role IN ('admin','viewer')),
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE refresh_tokens (
  id          TEXT PRIMARY KEY,
  user_id     TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_hash  TEXT NOT NULL UNIQUE,              -- sha256(token)
  family_id   TEXT NOT NULL,                     -- rotation family; reuse of a revoked token revokes the family
  expires_at  TEXT NOT NULL,
  revoked_at  TEXT,
  user_agent  TEXT,
  created_at  TEXT NOT NULL
);
CREATE INDEX idx_refresh_user ON refresh_tokens(user_id);

CREATE TABLE key_pairs (
  id          TEXT PRIMARY KEY,                  -- key-<17 hex>
  name        TEXT NOT NULL UNIQUE,
  type        TEXT NOT NULL CHECK (type IN ('ed25519','rsa')),
  public_key  TEXT NOT NULL,                     -- authorized_keys line
  fingerprint TEXT NOT NULL,                     -- SHA256:base64 (OpenSSH style)
  imported    INTEGER NOT NULL DEFAULT 0,
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL
);

CREATE TABLE images (
  id               TEXT PRIMARY KEY,             -- img-<17 hex>
  name             TEXT NOT NULL UNIQUE,
  description      TEXT NOT NULL DEFAULT '',
  os_family        TEXT NOT NULL DEFAULT 'linux',
  os_version       TEXT NOT NULL DEFAULT '',
  architecture     TEXT NOT NULL DEFAULT 'x86_64' CHECK (architecture IN ('x86_64','aarch64')),
  default_user     TEXT NOT NULL DEFAULT '',     -- e.g. ubuntu, debian, rocky
  format           TEXT NOT NULL DEFAULT 'qcow2' CHECK (format IN ('qcow2','raw')),
  state            TEXT NOT NULL CHECK (state IN ('pending','downloading','available','failed','deleting')),
  state_reason     TEXT,
  progress         INTEGER NOT NULL DEFAULT 0,
  source_url       TEXT,
  checksum_algo    TEXT,
  checksum_value   TEXT,
  size_bytes       INTEGER,                      -- file size on disk
  virtual_size_bytes INTEGER,                    -- qcow2 virtual size
  min_disk_gb      INTEGER NOT NULL DEFAULT 0,
  volume_name      TEXT,                         -- libvirt volume in images pool (e.g. img-xxxx.qcow2)
  created_by       TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);

CREATE TABLE instance_types (
  id          TEXT PRIMARY KEY,                  -- kumo.small
  vcpus       INTEGER NOT NULL,
  memory_mib  INTEGER NOT NULL,
  description TEXT NOT NULL DEFAULT ''
);

CREATE TABLE vpcs (
  id              TEXT PRIMARY KEY,              -- vpc-<17 hex>; also libvirt_network by default
  name            TEXT NOT NULL UNIQUE,          -- immutable; seeds DNS domain
  cidr            TEXT NOT NULL,                 -- RFC 1918, /16 to /28, immutable
  mode            TEXT NOT NULL DEFAULT 'nat' CHECK (mode IN ('nat','isolated')), -- immutable
  gateway_ip      TEXT NOT NULL,                 -- first host IP of VPC CIDR (e.g. 10.0.0.1)
  netmask         TEXT NOT NULL,                 -- e.g. 255.255.0.0
  bridge_name     TEXT NOT NULL UNIQUE,          -- kmb<8 hex> (or virbr-default for adopted)
  libvirt_network TEXT NOT NULL UNIQUE,          -- vpc-<17 hex> (or kumo-default for adopted)
  is_default      INTEGER NOT NULL DEFAULT 0,
  created_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL
);

CREATE TABLE subnets (
  id          TEXT PRIMARY KEY,                  -- subnet-<17 hex>
  vpc_id      TEXT NOT NULL REFERENCES vpcs(id), -- parent VPC
  name        TEXT NOT NULL,                     -- unique per VPC, immutable
  cidr        TEXT NOT NULL,                     -- contained within VPC CIDR, /16 to /28, immutable
  is_default  INTEGER NOT NULL DEFAULT 0,
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL,
  UNIQUE (vpc_id, name)
);
CREATE INDEX idx_subnets_vpc ON subnets(vpc_id);

CREATE TABLE instances (
  id             TEXT PRIMARY KEY,               -- i-<17 hex>; also the libvirt domain name
  name           TEXT NOT NULL,
  state          TEXT NOT NULL CHECK (state IN
                  ('pending','running','stopping','stopped','terminating','terminated','error')),
  state_reason   TEXT,
  image_id       TEXT NOT NULL REFERENCES images(id),
  instance_type  TEXT NOT NULL REFERENCES instance_types(id),
  vcpus          INTEGER NOT NULL,               -- snapshot at launch
  memory_mib     INTEGER NOT NULL,
  root_disk_gb   INTEGER NOT NULL,
  key_pair_id    TEXT REFERENCES key_pairs(id) ON DELETE SET NULL,
  key_pair_name  TEXT,                           -- kept for display after key deletion
  user_data      TEXT,
  libvirt_uuid   TEXT,
  mac_address    TEXT,                           -- snapshot of the address the domain was given
  private_ip     TEXT,                           -- snapshot of the address the domain was given
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  launched_at    TEXT NOT NULL,
  terminated_at  TEXT,                           -- only ever set on rows an earlier release kept
  updated_at     TEXT NOT NULL
);
CREATE INDEX idx_instances_state ON instances(state);
CREATE INDEX idx_instances_image ON instances(image_id);
-- The live-name rule. A terminate deletes the row, so this only matters for the
-- rows an earlier release left in 'terminated'.
CREATE UNIQUE INDEX idx_instances_name_live ON instances(name) WHERE state <> 'terminated';

CREATE TABLE network_interfaces (
  id           TEXT PRIMARY KEY,                 -- eni-<17 hex>
  instance_id  TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
  subnet_id    TEXT NOT NULL REFERENCES subnets(id),
  device_index INTEGER NOT NULL DEFAULT 0,       -- 0 for eth0, modelled for multi-NIC later
  mac_address  TEXT NOT NULL UNIQUE,             -- 52:54:00:xx:xx:xx
  private_ip   TEXT NOT NULL,                    -- allocated static IP in subnet
  created_at   TEXT NOT NULL,
  UNIQUE (instance_id, device_index)
);
CREATE INDEX idx_network_interfaces_subnet ON network_interfaces(subnet_id);
CREATE UNIQUE INDEX idx_network_interfaces_ip_live ON network_interfaces(subnet_id, private_ip);

CREATE TABLE tags (
  resource_id TEXT NOT NULL,
  key         TEXT NOT NULL,
  value       TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (resource_id, key)
);

CREATE TABLE tasks (
  id          TEXT PRIMARY KEY,                  -- task-<17 hex>
  type        TEXT NOT NULL,                     -- instance.launch, instance.terminate, image.import, bucket.delete, db.create, ...
  resource_id TEXT,
  state       TEXT NOT NULL CHECK (state IN ('pending','running','succeeded','failed')),
  progress    INTEGER NOT NULL DEFAULT 0,
  payload     TEXT NOT NULL DEFAULT '{}',        -- JSON input
  error       TEXT,
  attempts    INTEGER NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE INDEX idx_tasks_state ON tasks(state);
CREATE INDEX idx_tasks_resource ON tasks(resource_id);

CREATE TABLE settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- +goose Down
-- (drop in reverse order)
```

## Object storage (0004_buckets.sql)

```sql
CREATE TABLE buckets (
  name        TEXT PRIMARY KEY,                  -- the S3 bucket name, immutable
  state       TEXT NOT NULL DEFAULT 'available' CHECK (state IN ('available','deleting')),
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
```

- RustFS is the source of truth for which buckets exist (`06-object-storage.md`). A row holds only what Kumo adds. A bucket made with another client has no row until it is tagged or force-deleted through Kumo, and it is listed all the same.
- A row whose bucket has disappeared from the server is dropped, with its tags, the next time the buckets are listed. Otherwise a bucket recreated later under the same name would inherit a stranger's creator and tags.
- Bucket tags live in `tags` under the resource id `bucket/<name>`. A bucket is named, not numbered, and a bare name could equal another resource's id (`i-0123456789abcdef0` is a valid bucket name). The same id is the `resource_id` of its `bucket.delete` tasks.
- There is no bucket id format. The name is the identity, as in S3, and it is immutable.

## Access keys (0005_access_keys.sql)

```sql
CREATE TABLE access_keys (
  access_key  TEXT PRIMARY KEY,                  -- KUMO + 16 base32 characters, the S3 access key id
  name        TEXT NOT NULL UNIQUE,
  level       TEXT NOT NULL CHECK (level IN ('read-only','read-write')),
  created_by  TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at  TEXT NOT NULL,
  updated_at  TEXT NOT NULL
);
CREATE TABLE access_key_buckets (
  access_key TEXT NOT NULL REFERENCES access_keys(access_key) ON DELETE CASCADE,
  bucket     TEXT NOT NULL,
  PRIMARY KEY (access_key, bucket)
);
```

- **No secrets here.** The secret key is never stored; the create answer is the only place it exists.
- **Each row is a RustFS user** with the access key as its name, plus a policy named `kumo-<lowercased access key>`. RustFS is the truth for whether the key works, and the API reports a key RustFS has lost as `missing`.
- **No foreign key from `access_key_buckets.bucket`:** buckets have no row of their own until Kumo changes them (`buckets` above). Deleting a bucket leaves the grant in place.
- **The access key is the id.** It is a credential id in the shape S3 clients expect, not an `<prefix>-<17 hex>` resource id.

## Engine images (0006_engine_images.sql)

```sql
ALTER TABLE images ADD COLUMN kind TEXT NOT NULL DEFAULT 'user'
  CHECK (kind IN ('user','engine'));
ALTER TABLE images ADD COLUMN engine TEXT;
ALTER TABLE images ADD COLUMN engine_version TEXT;
CREATE INDEX idx_images_engine ON images(kind, engine, engine_version);
```

- An **engine image** is an ordinary image the user does not launch: `kind='engine'` marks a golden image the databases module boots (`07-databases.md` §7). `engine`/`engine_version` are set on one and `NULL` otherwise, and they are how a create request resolves "Postgres 18" to a volume.
- Every image list query filters on `kind`, and the service substitutes `user` for an empty filter, so a caller that does not think about it cannot publish engine images on a page the console reaches. `GET /images/{id}` still serves one: an operator debugging a database needs the row.
- The columns arrive in a migration of their own rather than with `db_instances` below, because the two were separate tasks and an applied migration is never edited.

## DB instances (0007_db_instances.sql)

```sql
ALTER TABLE instances ADD COLUMN managed_by TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_instances_managed ON instances(managed_by);

CREATE TABLE db_instances (
  id             TEXT PRIMARY KEY,               -- db-<17 hex>
  identifier     TEXT NOT NULL,                  -- user-chosen, immutable; seeds the endpoint name
  state          TEXT NOT NULL CHECK (state IN
                  ('creating','available','starting','stopping','stopped',
                   'rebooting','deleting','failed')),
  state_reason   TEXT,
  engine         TEXT NOT NULL CHECK (engine IN ('postgres')),
  engine_version TEXT NOT NULL,
  image_id       TEXT NOT NULL REFERENCES images(id),
  instance_id    TEXT NOT NULL REFERENCES instances(id),
  instance_type  TEXT NOT NULL REFERENCES instance_types(id),
  storage_gb     INTEGER NOT NULL,               -- the data disk, not the root disk
  port           INTEGER NOT NULL DEFAULT 5432,
  admin_username TEXT NOT NULL,
  database_name  TEXT NOT NULL,
  endpoint       TEXT,                           -- <identifier>.<vpc name>
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);
CREATE UNIQUE INDEX idx_db_instances_identifier ON db_instances(identifier);
CREATE UNIQUE INDEX idx_db_instances_instance ON db_instances(instance_id);
```

- **`instances.managed_by`** says which Kumo module owns a VM: `''` is a user's own instance, `'rds'` is one the databases module created. Every instance list and search query filters on it, so a managed VM cannot appear on the Instances pages, while capacity accounting, the reconciler and the orphan report go on counting it exactly as before (`07-databases.md` R7). `AllocatedCapacity` and `ListInstancesForReconcile` are deliberately unfiltered for that reason.
- **No password column, and no password hash column.** The verifier kumod computes from the admin password is written into the guest's cloud-init seed and then dropped; nothing persists either. A `db_instances` row therefore cannot be used to authenticate to its own database, which is the point.
- **No `vcpus`/`memory_mib` snapshot.** The backing `instances` row already snapshots them at launch, and that row is the one capacity sums.
- **`instance_id` is `NOT NULL` with no cascade.** A DB instance without its server is not a state worth representing, so a delete removes the two rows in one transaction, `db_instances` first because of the foreign key.
- **`identifier` is immutable**, because it is the endpoint. It is also the backing instance's name, so it shares `idx_instances_name_live` with every user instance: an identifier that collides with a live instance name is refused.
- Tags are not wired up for a DB instance in this version. The generic `tags` table keyed on `db-<id>` is what it would use.

## Notes

- A terminate **deletes** the `instances` row rather than keeping it as history (`01-api.md`): its `network_interfaces` rows follow through the cascade and its tags go in the same transaction, so a terminated instance leaves nothing behind but its tasks and its id, which stays allocated. Rows an earlier release left in `terminated` are the exception and stay until an operator `DELETE`s that id — the `state` CHECK still accepts the value and the partial unique index still releases the name, so such a row neither blocks a launch nor needs a migration of its own to disappear.
- `network_interfaces` exists exactly as long as its instance does: its rows are the cascade of the `instances` row delete, which a terminate makes in one transaction with the tags. There is no allocation history left to preserve in `instances.private_ip` and `instances.mac_address`; those go with the row that held them.
- `key_pair_name`/`vcpus`/`memory_mib` are denormalized snapshots, so a row keeps what it was launched with even after the key pair is deleted or the preset changes.
- `instance_types` is managed by administrators (`01-api.md` §Instance types). Migration `0003_seed_instance_types.sql` writes the `kumo.*` presets once with `INSERT OR IGNORE` and has a no-op `Down`; nothing seeds them at boot, so a deleted preset stays deleted. Deleting a type is refused (`409`) while **any** `instances` row references it, for the same foreign-key reason as images below. The `id` is immutable.
- `vpcs` and `subnets`: immutable attributes (`cidr`, `mode`, `name`, `vpc_id`) prevent accidental disruption of active routing and DNS domains. Only `tags` are editable.
- Consistency from transactions: IP allocation, interface creation, and capacity reservation execute within a single SQLite transaction (single writer in WAL mode). A launch cannot slip into a subnet or VPC that a delete has just checked.
- Tags: max 50/resource, key ≤ 128, value ≤ 256 chars. Orphan tags are cleaned when a resource is hard-deleted.
- Deleting an image is refused (`409`) while **any** `instances` row references it, terminated ones included: a running instance still boots from the volume, and `instances.image_id` is `NOT NULL REFERENCES images(id)` with no `ON DELETE` clause, so removing a referenced image would fail on the foreign key *after* its volume was already gone. There is no `deleted` tombstone state in v1; if one is ever wanted, add it to the `state` CHECK enum in a new migration and keep the row with `volume_name = NULL`.
- Never store private keys. `user_data` is stored plaintext (document that users shouldn't put long-lived secrets in it; same caveat as AWS).
