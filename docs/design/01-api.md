# 01 — REST API (v1)

Base path: `/api/v1`. JSON only. Auth: `Authorization: Bearer <access_jwt>` except where noted.
Source of truth for schemas is `api/openapi.yaml` (to be written in M0 from this doc).

## Conventions

- Resource IDs: `i-…`, `img-…`, `key-…`, `vpc-…`, `subnet-…`, `task-…`.
- Timestamps: RFC3339 UTC.
- List endpoints: `?page_size=50&page_token=…&sort=created_at:desc&q=<name substring>`; response `{ "items": [...], "next_page_token": "..." }`.
- Async operations return `202 Accepted` with the resource (transitional state) and `Location: /api/v1/tasks/<id>`.
- Error envelope (all non-2xx):

```json
{ "error": { "code": "invalid_argument", "message": "name is required",
             "details": [ { "field": "name", "reason": "required" } ],
             "request_id": "01J..." } }
```

Codes: `invalid_argument, unauthenticated, permission_denied, not_found, already_exists, conflict, quota_exceeded, internal, unavailable`.

## Auth

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/auth/setup` | none, only if zero users | Create first admin `{username,password}` |
| POST | `/auth/login` | none | `{username,password}` → `{access_token,expires_in,user}`; sets refresh cookie |
| POST | `/auth/refresh` | refresh cookie | Rotates refresh cookie, returns new access token |
| POST | `/auth/logout` | cookie | Revokes refresh token, clears cookie |
| GET | `/auth/me` | JWT | Current user |

Access JWT claims: `sub` (user id), `role`, `iat`, `exp`, `jti`. Password policy: ≥ 10 chars. Login is rate-limited (5/min/IP+username) with uniform error to avoid user enumeration.

## System

| Method | Path | Description |
|---|---|---|
| GET | `/healthz`, `/readyz` | (no auth, outside `/api/v1`) |
| GET | `/api/v1/system/info` | (no auth) `{version, needs_setup, hypervisor: {type, ok, kvm}, object_storage: {backend, ok}, databases: {enabled, engines}, kubernetes: {enabled, versions}}` (`kvm` is null when the backend cannot answer; `backend` is `none` when object storage is off, see `06-object-storage.md`; `databases.engines` is `[{engine, version}]` and is empty until an engine image is imported, see `07-databases.md`; `kubernetes.versions` is the Kubernetes minors with an available image, newest first, see `08-kubernetes.md`) |
| GET | `/api/v1/system/capacity` | Host CPU/memory/disk totals vs allocated; see `00-architecture.md` §6 |
| GET | `/api/v1/system/orphans` | (admin) Domains running under an instance id that no row claims, from the last reconcile pass; see `00-architecture.md` §4.3. Reported, never removed |

## Instance types

| Method | Path | Description |
|---|---|---|
| GET | `/instance-types` | List types, smallest first (memory, then vCPUs, then id) |
| POST | `/instance-types` | (admin) Create a type → `201` with `Location` |
| GET | `/instance-types/{id}` | Type detail plus `used_by` (ids of the instances that reference it) |
| PATCH | `/instance-types/{id}` | (admin) Change `vcpus`, `memory_mib` and/or `description` |
| DELETE | `/instance-types/{id}` | (admin) `204`; `409` while any instance references it |

Seed set: `kumo.nano` (1 vCPU/512 MiB), `kumo.micro` (1/1 GiB), `kumo.small` (1/2 GiB), `kumo.medium` (2/4 GiB), `kumo.large` (2/8 GiB), `kumo.xlarge` (4/16 GiB), `kumo.2xlarge` (8/32 GiB).

The seed set is written **once**, by migration `0003_seed_instance_types.sql` (`INSERT OR IGNORE`, so a node that already had the presets keeps them, edits included). From then on the presets are ordinary rows an administrator may edit or delete; a deleted preset does not come back on restart.

```json
{ "id": "db.large", "vcpus": 4, "memory_mib": 8192, "description": "4 vCPU, 8 GiB" }
```

Validation: `id` 1–64 chars `^[a-z0-9][a-z0-9._-]*$`, immutable (instances reference it; there is no rename); `vcpus` 1–256; `memory_mib` 256–1048576; `description` ≤ 255 bytes. A taken `id` is `409 already_exists`. The bounds keep a type launchable, not sized to this node: whether a launch fits is the capacity check (`00-architecture.md` §6), so a type larger than the host is legal and refused at launch.

Description: an empty `description` on create is generated from the size the way the presets read (`"4 vCPU, 8 GiB"`, or `"1 vCPU, 1536 MiB"` for a size that is not whole GiB). On `PATCH`, a description that still equals the generated one for the old size follows the new size; a hand-written one is left alone.

Editing a type never touches existing instances: they snapshot `vcpus`/`memory_mib` at launch (`02-data-model.md`), so a change applies to later launches only. A delete is refused while **any** `instances` row references the type, terminated ones included, because `instances.instance_type` is a foreign key with no `ON DELETE` (the same rule as deleting an image).

## Key pairs

| Method | Path | Description |
|---|---|---|
| GET | `/key-pairs` | List |
| POST | `/key-pairs` | Create: server generates **ed25519** (or `rsa-4096`), returns private key **once** |
| POST | `/key-pairs/import` | Import `{name, public_key}` (OpenSSH format) |
| GET | `/key-pairs/{id}` | Detail (includes `used_by` instance IDs) |
| DELETE | `/key-pairs/{id}` | Delete. `409` if in use by a non-terminated instance unless `?force=true` (deleting doesn't alter running instances, as in AWS) |
| PATCH | `/key-pairs/{id}` | Update tags only |

Create request: `{ "name": "my-key", "type": "ed25519", "tags": {"env":"dev"} }`

Create response (`201`):
```json
{ "id": "key-0a1b2c3d4e5f60718", "name": "my-key", "type": "ed25519",
  "fingerprint": "SHA256:abcd...", "public_key": "ssh-ed25519 AAAA... my-key",
  "private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n...", "created_at": "..." }
```
`private_key` is present **only** in this response. Name rules: `^[A-Za-z0-9._-]{1,64}$`, unique (`409` otherwise). Imported public keys are validated with `golang.org/x/crypto/ssh.ParseAuthorizedKey`; RSA < 2048 bits rejected; private key material pasted into the public-key field is rejected.

## Images

| Method | Path | Description |
|---|---|---|
| GET | `/images` | List. Engine images are never returned: they belong to the databases module, not the Images pages (`07-databases.md`) |
| POST | `/images/import` | Import from URL (async, `202`). `kind: "engine"` with `engine` and `engine_version` imports a golden image for the databases module |
| POST | `/images/upload` | Multipart upload of qcow2/raw/img (async finalize) — *M3b, optional* |
| GET | `/images/{id}` | Detail |
| PATCH | `/images/{id}` | Update `name`, `description`, `tags` |
| DELETE | `/images/{id}` | Delete. `409` if any instance references it, terminated ones included (see `02-data-model.md`) |
| POST | `/images/{id}/retry` | Retry the last failed import or delete (async, `202`). `409` unless the image is `failed` |
| GET | `/images/catalog` | Curated well-known cloud images (Ubuntu, Debian, Rocky, Alma, Fedora, Alpine) with URLs & checksum URLs, to power a "quick import" picker |

Import request:
```json
{ "name": "ubuntu-24.04", "description": "Ubuntu Noble cloud image",
  "source_url": "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
  "checksum": { "algorithm": "sha256", "value": "…" },
  "os_family": "ubuntu", "os_version": "24.04", "architecture": "x86_64",
  "default_user": "ubuntu", "min_disk_gb": 10 }
```
Every image carries a `kind`, `user` by default. An import that omits it is an ordinary image,
which is what every client written before the databases module sends. A golden image for that
module is imported with

```json
{ "name": "postgres-18", "source_url": "https://.../kumo-postgres-18-20261028.qcow2.gz",
  "checksum": { "algorithm": "sha256", "value": "..." },
  "kind": "engine", "engine": "postgres", "engine_version": "18", "min_disk_gb": 6 }
```

Any import may be **gzipped**: a download that starts with the gzip magic bytes is decompressed
after the checksum is checked, so the checksum is of the `.gz`, as published. The URL's extension
does not matter. The decompressed image is bounded by `KUMO_IMAGE_MAX_BYTES` like a download, and
a stream that is not valid gzip fails the import. The volume is always stored uncompressed.

and `engine`/`engine_version` are required with `kind: "engine"` and refused without it. `engine` is
`postgres` (a database engine, versioned by its major, `18`) or `kubernetes` (the Kubernetes
module's node image, versioned by its minor, `1.37`; `08-kubernetes.md` §7). A Kubernetes image is
never offered as a database: `/system/info`'s `databases.engines` lists database engines only. An engine
image is reachable at `GET /images/{id}` but never appears in `GET /images`, so the Images pages
cannot offer a machine nobody should launch directly; `GET /system/info` reports which engines a
node can create a DB instance from (`07-databases.md`).

Image states: `pending → downloading → available`, `failed`, `deleting`.
`progress` is a percentage of the server's `Content-Length`, so it stays `0` while a chunked
download streams — most image CDNs answer that way — and jumps to `100` when the image is
published. The state is the signal that work is happening; a progress bar built on top of it
should show the state until a percentage exists (M3.3).

## Instances

| Method | Path | Description |
|---|---|---|
| GET | `/instances` | List (filter `?state=running&q=web`, `?page_size=`, `?page_token=`, `?sort=launched_at:asc`) |
| POST | `/instances` | Launch (async, `202`) |
| GET | `/instances/{id}` | Detail |
| PATCH | `/instances/{id}` | Update `name`, `tags` |
| POST | `/instances/{id}/start` | `202` if `stopped` |
| POST | `/instances/{id}/stop` | ACPI graceful shutdown; `?force=true` → destroy |
| POST | `/instances/{id}/reboot` | ACPI reboot; `?force=true` → reset |
| DELETE | `/instances/{id}` | Terminate: destroy domain, delete disks. `202` |
| GET | `/instances/{id}/console-output` | Last N KiB of serial console log (`?tail_kib=`, admin only) |
| GET | `/instances/{id}/console` | WebSocket → VNC proxy (*M5*, admin only, `501` until then) |

**Managed instances.** The virtual machine behind a DB instance is an ordinary
row in `instances` that the databases module owns (`07-databases.md` R7). It is
**not returned by `GET /instances`** and not counted in the dashboard tallies,
but it is counted by `GET /system/capacity`, because it holds real vCPUs,
memory and disk. `GET /instances/{id}` and `console-output` still serve it, so
an operator can debug a database that will not start; `PATCH`, `start`, `stop`,
`reboot` and `DELETE` answer **`409`** naming the DB instance and the
`/rds/db-instances` path to use instead. A viewer gets `403` first, as
everywhere.

Launch request:
```json
{ "name": "web-1", "image_id": "img-…", "instance_type": "kumo.small",
  "subnet_id": "subnet-…",
  "key_pair_id": "key-…", "root_disk_gb": 20,
  "user_data": "#cloud-config\npackages: [nginx]\n",
  "count": 1, "tags": {"env":"dev"} }
```
Validation: `name` 1–63 chars RFC-1123 label-ish; `subnet_id` optional (defaults to the default VPC's default subnet if omitted); if provided, subnet must exist; `root_disk_gb` ≥ image `min_disk_gb` (default: the image's virtual size rounded up), ≤ 2048; `user_data` ≤ 16 KiB; `count` must be 1 (a count above 1 is a `400` naming `count`: the field is in the contract for a release that launches several instances from one image, and naming them — `i-2-web`? `web-2`? — is a convention this document does not define, so the launch is refused rather than invented); image must be `available`; the instance type must exist; the host capacity check (`429` `quota_exceeded`).

`POST /instances` answers `202` with the instance in `pending` and a `Location` header pointing at
the task that tracks the launch. The boot itself (root disk, seed ISO, domain, address) happens in
the worker; see `04-libvirt-integration.md` §3.

### Lifecycle actions

`POST /instances/{id}/start`, `POST /instances/{id}/stop`, `POST /instances/{id}/reboot` and
`DELETE /instances/{id}` all answer `202` with the instance and a `Location` header pointing at the
task that tracks the work. None of them blocks on the guest: a boot, an ACPI shutdown and a
teardown all take longer than an HTTP request, so the response says the action was *accepted* and
the state it left behind, and the state changes again when the task finishes.

Each action is only accepted from the states that make sense for it. That is the state machine of
`00-architecture.md` §4, not a guess about what the operator probably meant:

| Action | Accepted from | State written when accepted |
|---|---|---|
| `start` | `stopped` | none: the row stays `stopped` until the guest is up (`pending` means a launch) |
| `stop` | `running`, `stopping` | `stopping` |
| `reboot` | `running` | none |
| `DELETE` | `pending`, `running`, `stopping`, `stopped`, `terminating`, `terminated`, `error` | `terminating` |

A `stop` that leaves a guest which ignores its ACPI request times out and leaves the row in
`stopping`; `stopping` is therefore also a state a stop may be asked from, so `?force=true` is the
way out. `DELETE` removes the instance rather than marking it: the task destroys the domain, deletes
its storage and its NVRAM file, deletes the row (its interface rows and tags go with it, its console
log file beside it) and leaves the tasks as the record of what happened. `GET /instances/{id}`
answers `404` once it has run. A repeat of the call is a `404` like any other unknown id: the work
is done and there is nothing left to queue, and a client that never saw the first answer learns the
same thing from it. `terminated` is still a state a `DELETE` is accepted from, for the rows an
earlier release kept as history — the task purges those, so an operator can clear them without a
migration that deletes history on its own.

`?force=true` is the destructive shortcut: a forced stop destroys the domain instead of asking the
guest, and a forced reboot resets it instead of asking the guest to restart. It has to be a
boolean: `?force=maybe` is a `400` about `force`, not a `false`, because a stop that was meant to
be forced would otherwise come back as a graceful request the guest can refuse.

`409 conflict` means the request was well formed and the instance was not in a state to accept it.
Three things produce it, and all three say in the message what to do instead: a lifecycle action
that is already queued or running for that instance (the worker holds a per-instance lock while it
works on one, and a task row that is still `pending` or `running` counts even when no worker holds
the lock, e.g. after a restart), a state the action does not apply to, and a name that
`PATCH /instances/{id}` cannot take because a live instance already has it.

`PATCH /instances/{id}` renames and re-tags, and nothing else: the CPU count, the disk size and the
image an instance boots from are inputs of a launch, and changing them on a running instance would
claim something the host is not doing. A body that carries neither a `name` nor a `tags` is a `400`
about `body`. An absent `tags` leaves the tags alone and `{}` drops every tag, because the console
sends the whole set and a merge would keep a tag the operator deleted in the form.

`GET /instances/{id}/console-output` answers `{ "instance_id": "i-…", "output": "…" }` with the
last `tail_kib` KiB of the serial console log (1–1024, default 64; anything else is a `400` about
`tail_kib`). The bytes come from the file the domain writes through libvirt's `<log>` element, so
an instance whose domain is gone is a `404` rather than an empty log, and a definition an operator
edited still reports the log it writes. Both this endpoint and `GET /instances/{id}/console` are
administrator-only: the console of a guest carries whatever its cloud-init printed.

`GET /instances/{id}/console` is registered and answers `501` (`unavailable`) until the VNC proxy
of M5 exists, after checking that the instance does: a missing instance is a `404`, because a
client that cannot tell "not built yet" from "no such instance" cannot recover from either.

Instance resource:
```json
{ "id": "i-…", "name": "web-1", "state": "running", "state_reason": null,
  "image_id": "img-…", "image_name": "ubuntu-2404", "instance_type": "kumo.small",
  "vcpus": 1, "memory_mib": 2048, "root_disk_gb": 20, "key_pair_id": "key-…",
  "key_pair_name": "laptop", "vpc_id": "vpc-…", "subnet_id": "subnet-…",
  "private_ip": "10.0.1.10", "mac_address": "52:54:00:…",
  "network_interfaces": [
    { "id": "eni-…", "device_index": 0, "mac_address": "52:54:00:…",
      "private_ip": "10.0.1.10", "subnet_id": "subnet-…", "vpc_id": "vpc-…" }
  ],
  "user_data": null, "launched_at": "…",
  "terminated_at": null, "updated_at": "…", "tags": {}, "task_id": "task-…" }
```
`user_data` is never returned by list; detail returns it only to admins. The libvirt uuid is on the
row, not in the response: it is a backend detail the reconciler matches domains with, not something a
user acts on. `network_interfaces` returns the instance's interfaces (currently 1, modelled for more).
On termination, the interface row is deleted while `private_ip` and `mac_address` remain on the
instance for history.

## VPCs

One VPC corresponds to one libvirt network and one Linux bridge.

| Method | Path | Description |
|---|---|---|
| GET | `/vpcs` | List (`?page_size=`, `?page_token=`, `?sort=`, `?q=`) |
| POST | `/vpcs` | Create (async, `202`, defines and starts libvirt network) |
| GET | `/vpcs/{id}` | Detail (includes subnet count, instance count) |
| PATCH | `/vpcs/{id}` | Update tags only (`name`, `cidr`, `mode` are immutable) |
| DELETE | `/vpcs/{id}` | Delete (async, `202`). `409` if any subnets or instances exist |

Create VPC request:
```json
{ "name": "production", "cidr": "10.0.0.0/16", "mode": "nat", "tags": {"env": "prod"} }
```
- `cidr`: required, RFC 1918 IPv4 between `/16` and `/28`. Must not overlap another VPC or any host interface address or route (excluding Kumo's own bridges).
- `mode`: `nat` (libvirt forward mode `nat`) or `isolated` (no forward element). Default `nat`.
- `name`: 1–63 chars, RFC-1123 label-ish, unique (`409` otherwise). The name seeds the DNS domain. Immutable once created.
- `cidr`, `mode`, and `name` are immutable. Only `tags` are patchable.

Create VPC response (`201` or `202` with `Location: /api/v1/tasks/<id>`):
```json
{ "id": "vpc-0a1b2c3d4e5f60718", "name": "production", "cidr": "10.0.0.0/16",
  "mode": "nat", "gateway_ip": "10.0.0.1", "netmask": "255.255.0.0",
  "bridge_name": "kmb0a1b2c3d", "libvirt_network": "vpc-0a1b2c3d4e5f60718",
  "is_default": false, "created_at": "…", "updated_at": "…", "tags": {"env": "prod"} }
```

Validation rules:
- VPC CIDR must be valid RFC 1918 (`10.0.0.0/8`, `172.16.0.0/12`, or `192.168.0.0/16`) and prefix length between `/16` and `/28`.
- Must not overlap any other active VPC's CIDR.
- Must not overlap any host interface address or route from the host's routing table (excluding Kumo's own bridges `kmb*` / `virbr-*`).
- `gateway_ip` is automatically the first host address in the VPC CIDR (e.g. `10.0.0.1` for `10.0.0.0/16`).
- Bridge name is `kmb` + first 8 hex characters of the VPC ID (fits Linux `IFNAMSIZ` 15 chars).
- Deletion requires all subnets to be deleted first. If any subnets or live interfaces exist, delete is rejected with `409 conflict`.

## Subnets

Subnets are logical allocation partitions of their parent VPC's bridge. No L2 separation, no inter-subnet routing or filtering. Guests receive the VPC's netmask, so the gateway (`gateway_ip`) belongs to the VPC.

| Method | Path | Description |
|---|---|---|
| GET | `/subnets` | List (`?vpc_id=…`, `?page_size=`, `?page_token=`, `?sort=`, `?q=`) |
| POST | `/subnets` | Create (synchronous, `201`) |
| GET | `/subnets/{id}` | Detail (includes allocated IP count, available IP count) |
| PATCH | `/subnets/{id}` | Update tags only (`name`, `cidr`, `vpc_id` are immutable) |
| DELETE | `/subnets/{id}` | Delete. `409` if any active network interfaces reference it |

Create Subnet request:
```json
{ "vpc_id": "vpc-0a1b2c3d4e5f60718", "name": "public-1", "cidr": "10.0.1.0/24", "tags": {} }
```

Create Subnet response (`201`):
```json
{ "id": "subnet-0a1b2c3d4e5f60718", "vpc_id": "vpc-0a1b2c3d4e5f60718",
  "name": "public-1", "cidr": "10.0.1.0/24", "total_ips": 256,
  "usable_ips": 253, "allocated_ips": 0, "is_default": false,
  "created_at": "…", "updated_at": "…", "tags": {} }
```

Validation rules:
- `vpc_id` must reference an existing VPC.
- `cidr` must be fully contained within the parent VPC's CIDR, between `/16` and `/28`.
- `cidr` must not overlap any sibling subnet in the same VPC (`409 already_exists` / `conflict`).
- `name` 1–63 chars, unique within the VPC. Immutable.
- Usable addresses: total IPs minus 2 (network address and broadcast address) minus 1 if the VPC's `gateway_ip` falls within this subnet. IPv4 only.
- Delete is rejected (`409`) if any active instance/network interface is allocated from this subnet.

Consistency:
- State check and write share a single SQLite transaction (single writer), so a launch cannot slip into a subnet that a delete has just checked.
- Kumo selects the lowest free usable IP in the subnet during the launch transaction.
- dnsmasq only serves what Kumo allocates: a static `<host mac ip name>` entry is written to libvirt network before guest boot. No dynamic DHCP range is configured.

`state_reason` carries why an instance is in `error` — a failed launch writes the reason it failed
with (the step, and the error from the backend). `task_id` is the task of the most recent state
change, so the console follows a launch without guessing which task it was.

## Object storage (S3)

The optional module of `06-object-storage.md`. Every `/s3` route answers `503 unavailable` when `KUMO_OBJECTSTORE` is `none`, after authentication, so a client learns the module is off rather than that the path is wrong. A server that does not answer is also `503`.

| Method | Path | Description |
|---|---|---|
| GET | `/s3/buckets` | Every bucket on the server, by name, including buckets made with another client; not paged |
| POST | `/s3/buckets` | (admin) `{name, versioning?, tags?}` → `201` with `Location` and the bucket detail |
| GET | `/s3/buckets/{name}` | Detail: the bucket plus `versioning` (`off`/`enabled`/`suspended`), `endpoint` and `region` |
| PATCH | `/s3/buckets/{name}` | (admin) `versioning` and/or `tags`; the name is immutable |
| DELETE | `/s3/buckets/{name}` | (admin) `204` when empty, `409` otherwise; `?force=true` → `202` with a `bucket.delete` task |
| GET | `/s3/buckets/{name}/objects` | One folder: `?prefix=&cursor=&limit=` (1–1000, default 200) → `{prefix, folders, objects, next_cursor}` |
| POST | `/s3/buckets/{name}/uploads` | (admin) `{key, content_type?}` → presigned `PUT` `{url, method, headers, expires_at}` |
| POST | `/s3/buckets/{name}/downloads` | `{key}` → presigned `GET` that saves under the key's base name; `404` for a missing key |
| POST | `/s3/buckets/{name}/folders` | (admin) `{prefix}` → `201 {prefix}` with a trailing `/`; writes the zero-byte marker |
| POST | `/s3/buckets/{name}/objects/delete` | (admin) `{keys: [...]}`, 1–1000 → `204`; a folder key removes only its marker |

```json
{ "name": "app-logs", "state": "available", "created_at": "…", "created_by": "ada",
  "tags": {"env": "prod"}, "versioning": "enabled",
  "endpoint": "https://s3.kumo.example", "region": "us-east-1" }
```

Names follow the S3 rules: 3–63 characters of lowercase letters, digits, `.` and `-`, starting and ending with a letter or digit, no `..`, not an IP address, none of the reserved prefixes (`xn--`, `sthree-`, `amzn-s3-demo-`) or suffixes (`-s3alias`, `--ol-s3`, `--x-s3`, `.mrap`). A taken name is `409 already_exists`, also when the bucket was made outside Kumo. A name that cannot exist in the path of a `GET`/`PATCH`/`DELETE` is `404`.

`created_by` is the creator's username, or `null` for a bucket made outside Kumo. The first `PATCH` of such a bucket gives it a row (`02-data-model.md`). A create also enables versioning when asked and sets the console's CORS rule (`06-object-storage.md` §3). If either step fails, the bucket is removed again and the error returned.

Versioning: `false` suspends a bucket that had versioning and leaves a never-versioned bucket `off`, because S3 has no way back to `off`.

Delete: a bucket that has had versioning keeps old versions and delete markers after its objects are deleted, so it can look empty and still be refused with `409`. `?force=true` marks it `deleting` and queues `bucket.delete`, which removes every version and delete marker and then the bucket. While a bucket is `deleting`, `PATCH` and a plain `DELETE` are `409`. Another forced delete queues the task again, which is how a failed one is retried. An upload that lands between the emptying and the delete fails the task and leaves the bucket `deleting`.

### Access keys

| Method | Path | Description |
|---|---|---|
| GET | `/s3/access-keys` | Every key Kumo issued, by name; no secrets |
| POST | `/s3/access-keys` | (admin) `{name, level, buckets}` → `201` with the key and its `secret_key`, once |
| GET | `/s3/access-keys/{id}` | One key, without its secret |
| PATCH | `/s3/access-keys/{id}` | (admin) `name`, `level` and/or `buckets` |
| DELETE | `/s3/access-keys/{id}` | (admin) `204`; the key stops working at once |

```json
{ "access_key": "KUMO7Q2LZ4XKD3MPRW5A", "name": "web-app", "level": "read-write",
  "buckets": ["media"], "state": "active", "created_by": "ada",
  "created_at": "…", "updated_at": "…", "secret_key": "…only in the create answer…" }
```

- **Shape:** a key is S3 credentials for an application. The id is `KUMO` plus 16 base32 characters (80 random bits); the secret is 40 URL-safe characters (240 bits).
- **The secret is shown once.** It is returned in the create answer with `Cache-Control: no-store`, and never stored or logged, the same rule as a generated private key. It cannot be changed: delete a leaked key and issue a new one.
- **Levels:**
  - `read-only` lists the buckets and reads objects.
  - `read-write` also writes and deletes objects.
  - Neither can create, delete or reconfigure a bucket.
- **Validation:**
  - `name`: 1–64 characters of letters, digits, `.`, `_` and `-`, unique (`409` when taken).
  - `buckets`: 1–100 buckets that exist when the grant is made; an unknown bucket is a `400` on `buckets`.
- **Changing a key:** a change of `level` or `buckets` rewrites the key's policy on RustFS and applies to its next request.
- **Deleting a bucket keeps the grant.** Grants name buckets, not rows, so the grant reaches nothing until a bucket of that name exists again.
- **`state: missing`** means RustFS no longer has the key, because it was removed with another tool. Such a key does not work; it cannot be re-scoped (`409`), only deleted.
- **Listing:** RustFS users made with other tools are not Kumo's keys and are not listed.

Objects: the bytes of a console upload or download never pass through kumod. The console gets a presigned URL, valid for `KUMO_S3_PRESIGN_TTL`, and talks to RustFS at `KUMO_S3_PUBLIC_URL` directly.
- An upload URL signs its `Content-Type`, and the browser must send the `headers` given with it.
- The first upload to a bucket in a kumod process applies the console's CORS rule, because a bucket made outside Kumo has none.
- A download is a read, so a viewer may have its URL. It is a `POST` so that a URL granting access is never cached or logged as the address of a page.
- Keys are 1–1024 bytes of UTF-8, with no control characters and no leading `/`. An upload key must not end in `/`, which is a folder.
- Uploads, folders and deletes are `409` while the bucket is `deleting`. Downloads stay open.

## Databases (managed Postgres)

The module of `07-databases.md`. Every `/rds` route answers `503 unavailable` when `KUMO_DATABASES` is `off`, after authentication, so a client learns the module is off rather than that the path is wrong. The virtual machine behind a DB instance is Kumo's: it is absent from `GET /instances` and its own lifecycle endpoints answer `409` naming the DB instance (`07-databases.md` R7).

| Method | Path | Description |
|---|---|---|
| GET | `/rds/db-instances` | List, paged and sorted like every collection; `?q=` matches the identifier, `?state=` one state |
| POST | `/rds/db-instances` | (admin) Create → `202` with the DB instance `creating` and `Location: /api/v1/tasks/<id>` |
| GET | `/rds/db-instances/{id}` | Detail: the endpoint, the port, the admin username, the database name and the server's id |
| POST | `/rds/db-instances/{id}/stop` | (admin) → `202`. `409` unless `available`. An ACPI shutdown, so Postgres checkpoints |
| POST | `/rds/db-instances/{id}/start` | (admin) → `202`. `409` unless `stopped`. The endpoint does not move |
| POST | `/rds/db-instances/{id}/reboot` | (admin) → `202`. `409` unless `available` |
| DELETE | `/rds/db-instances/{id}` | (admin) → `202`, state `deleting`. Allowed from every state except `deleting` |

```json
{ "id": "db-0a1b2c3d4e5f60718", "identifier": "orders", "state": "available",
  "state_reason": null, "engine": "postgres", "engine_version": "18",
  "image_id": "img-…", "instance_id": "i-…", "instance_type": "kumo.medium",
  "vcpus": 2, "memory_mib": 4096, "storage_gb": 20, "port": 5432,
  "admin_username": "app", "database_name": "orders",
  "endpoint": "orders.default", "task_id": "task-…",
  "created_at": "…", "updated_at": "…" }
```

Create body:

```json
{ "identifier": "orders", "engine": "postgres", "engine_version": "18",
  "instance_type": "kumo.medium", "storage_gb": 20, "subnet_id": "subnet-…",
  "admin_username": "app", "admin_password": "…", "database_name": "orders" }
```

- **Nothing in a response ever carries the password, the verifier or the salt**, because none of them is stored. kumod derives a SCRAM-SHA-256 verifier from `admin_password`, writes it into the guest's cloud-init seed and discards the password (`07-databases.md` §5). So there is **no password reset** and no way to read it back; the console says so next to the password field.
- **Validation:** `identifier` is a DNS label of 1–60 characters that must not collide with a live instance name (`409 already_exists` either way, because the backing VM shares `idx_instances_name_live`); `admin_username` and `database_name` are unquoted Postgres identifiers of 1–63 characters and may not be `postgres`, a `pg_` prefix, `template0`, `template1`, `rdsadmin` or `kumo`; `storage_gb` is 5–2048 and at least the engine image's `min_disk_gb`; `admin_password` is 10–128 **printable ASCII** characters. The ASCII rule is a correctness rule rather than a convenience: SCRAM applies SASLprep before deriving, in the client and inside Postgres, and Kumo does not, so anything else would fail to authenticate with a correct password.
- `engine_version` may be omitted, which takes the newest available version; `subnet_id` may be omitted, which attaches the server the way a launch does. `port` is not accepted in this version.
- **`409 no_engine_image`** is the one error code outside the standard set of §Conventions. It means the module is on and no engine image is `available` yet, which is the normal state of a fresh node: the Databases pages explain the import rather than reporting a fault (`07-databases.md` §2).
- **`endpoint` is a DNS name**, `<identifier>.<vpc name>`, resolvable from inside the VPC and nowhere else: dnsmasq serves it for the static DHCP host Kumo writes. It is `null` until the guest has an address, and it survives a stop, a start and a reboot.
- **`state` is the database's, not the server's.** A guest that is running while the engine inside it has never answered is `creating` or `failed`, never `available`. What Kumo knows about the engine comes from a TCP connect and nothing else (`07-databases.md` §6), so `available` means "the port answers", not "your password works".
- **`state_reason`** carries the grep-able prefixes `engine_not_responding:` and `server_failed:` when the reconciler wrote the state.
- **`instance_id` is returned on purpose.** The server is not reachable through `/instances`, but the console log and the orphan report are keyed by its id, and hiding it would make the module harder to support without making it safer.
- **Delete takes the data with it.** The data disk is deleted with the server; there is no final snapshot and no recovery, which is what the console's confirmation says in as many words.

## Kubernetes clusters

The module of `08-kubernetes.md`. Every `/eks` route answers `503 unavailable` when `KUMO_KUBERNETES` is `off`, after authentication. A cluster's VMs are Kumo's: they are absent from `GET /instances` and their own lifecycle endpoints answer `409` naming the cluster (`08-kubernetes.md` K8).

| Method | Path | Description |
|---|---|---|
| GET | `/eks/clusters` | List, newest first, keyset-paged; `?q=` matches the name, `?state=` one state |
| POST | `/eks/clusters` | (admin) Create → `202` with the cluster `creating` and `Location: /api/v1/tasks/<id>` |
| GET | `/eks/clusters/{id}` | Detail: the endpoint, the CA certificate, the node group and every node with its Kubernetes `ready` |
| DELETE | `/eks/clusters/{id}` | (admin) → `202`, state `deleting`. Allowed from every state except `deleting` |
| GET | `/eks/clusters/{id}/node-groups` | The node groups: one, `default`, in this version |
| POST | `/eks/clusters/{id}/kubeconfig` | A kubeconfig for the caller, by role: `kumo:admins` or `kumo:viewers`, 30 days, returned once with `Cache-Control: no-store`. `409` until the cluster is active |

```json
{ "id": "eks-0a1b2c3d4e5f60718", "name": "orders", "kubernetes_version": "1.37",
  "state": "active", "state_reason": null,
  "endpoint": "https://api.orders.default:6443",
  "vpc_id": "vpc-…", "subnet_id": "subnet-…",
  "pod_cidr": "100.64.0.0/16", "service_cidr": "100.96.0.0/16",
  "control_plane_type": "kumo.medium", "ca_certificate": "-----BEGIN CERTIFICATE-----…",
  "node_groups": [{ "id": "ng-…", "name": "default", "instance_type": "kumo.medium",
                    "root_disk_gb": 8, "desired_size": 2 }],
  "nodes": [{ "name": "orders-cp-1", "instance_id": "i-…", "role": "control-plane",
              "node_group_id": null, "state": "running", "private_ip": "192.168.100.10", "ready": true }],
  "created_at": "…", "updated_at": "…" }
```

Create body: `{ "name": "orders", "kubernetes_version": "1.37", "subnet_id": "subnet-…", "control_plane_type": "kumo.medium", "node_type": "kumo.medium", "node_count": 2, "node_root_disk_gb": 20 }`; only `name` is required.

- **Validation:** `name` is a DNS label of 1–40 characters; the nodes are `<name>-cp-1` and `<name>-default-<n>`, so it must not collide with a live instance name (`409 already_exists`). `node_count` is 1–10 (default 2). `control_plane_type` needs at least 2 vCPU and `node_type` at least 2048 MiB, kubeadm's minimums; both default to `kumo.medium`. `node_root_disk_gb` is between the image's `min_disk_gb` (the default) and 500. Pod and service ranges are fixed: `100.64.0.0/16` and `100.96.0.0/16` (K9).
- **All or nothing.** The control plane and every worker are reserved in one transaction: a cluster the node cannot hold is `429 quota_exceeded` with nothing written, and the PKI generated for it is removed.
- **`409 no_engine_image`**, as for databases, means no `kubernetes` engine image is available.
- **States.** `active` once every node is Ready; `degraded` when the control plane answers but a node is not Ready, or after three failed health checks in a row (`state_reason` says which); `failed` when the API server never answered during the create, with the VMs kept for their console logs. Delete is the way out of both.
- **The endpoint** is `https://api.<name>.<vpc>:6443`, served by the VPC's dnsmasq as a DNS record kumod adds (K6): a kubeconfig works from an instance in the cluster's VPC, and nowhere else (K7).
- **No key in any response** but the kubeconfig's own client key, which is minted per call and never stored. `ca_certificate` is the public CA.

## Tasks

| Method | Path | Description |
|---|---|---|
| GET | `/tasks/{id}` | `{id,type,resource_id,state,progress,error,created_at,updated_at}` |
| GET | `/tasks?resource_id=…` | Recent tasks for a resource |

## Realtime (later)

`GET /events` — Server-Sent Events stream of `{type:"instance.state", id, state}`; the UI falls back to polling (5 s on pages with transitional resources, 30 s otherwise).

## Authorization matrix (v1)

| Role | Read | Create/Update/Delete |
|---|---|---|
| `admin` | ✅ | ✅ |
| `viewer` | ✅ (no `user_data`) | ❌ (`403`) |

Object storage follows the same matrix: both roles list and read buckets; every write is admin only. So does the databases module: both roles list and read DB instances, and creating, stopping, starting, rebooting and deleting one is admin only, because a DB instance claims the vCPUs, memory and disk of the node exactly as a launch does. And the Kubernetes module: both roles read clusters, creating and deleting one is admin only, and both roles may mint a kubeconfig -- a viewer's is in `kumo:viewers`, bound to `view`.
