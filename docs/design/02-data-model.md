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
  mac_address    TEXT,                           -- history snapshot; kept after termination
  private_ip     TEXT,                           -- history snapshot; kept after termination
  created_by     TEXT REFERENCES users(id) ON DELETE SET NULL,
  launched_at    TEXT NOT NULL,
  terminated_at  TEXT,
  updated_at     TEXT NOT NULL
);
CREATE INDEX idx_instances_state ON instances(state);
CREATE INDEX idx_instances_image ON instances(image_id);
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
  type        TEXT NOT NULL,                     -- instance.launch, instance.terminate, image.import, ...
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

## Notes

- `instances` keeps terminated rows for history (shown for a while in the UI, like AWS); the partial unique index lets names be reused after termination.
- `network_interfaces` exists exactly as long as its instance is not terminated: it is deleted in the same transaction that sets `terminated_at`, while `instances.private_ip` and `instances.mac_address` preserve the allocation history.
- `key_pair_name`/`vcpus`/`memory_mib` are denormalized snapshots so history survives deletions and preset changes.
- `vpcs` and `subnets`: immutable attributes (`cidr`, `mode`, `name`, `vpc_id`) prevent accidental disruption of active routing and DNS domains. Only `tags` are editable.
- Consistency from transactions: IP allocation, interface creation, and capacity reservation execute within a single SQLite transaction (single writer in WAL mode). A launch cannot slip into a subnet or VPC that a delete has just checked.
- Tags: max 50/resource, key ≤ 128, value ≤ 256 chars. Orphan tags are cleaned when a resource is hard-deleted.
- Deleting an image is refused (`409`) while **any** `instances` row references it, terminated ones included: a running instance still boots from the volume, and `instances.image_id` is `NOT NULL REFERENCES images(id)` with no `ON DELETE` clause, so removing a referenced image would fail on the foreign key *after* its volume was already gone. There is no `deleted` tombstone state in v1; if one is ever wanted, add it to the `state` CHECK enum in a new migration and keep the row with `volume_name = NULL`.
- Never store private keys. `user_data` is stored plaintext (document that users shouldn't put long-lived secrets in it; same caveat as AWS).
