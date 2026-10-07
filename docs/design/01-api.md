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
| GET | `/api/v1/system/info` | (no auth) `{version, needs_setup, hypervisor: {type, ok, kvm}}` (`kvm` is null when the backend cannot answer) |
| GET | `/api/v1/system/capacity` | Host CPU/memory/disk totals vs allocated; see `00-architecture.md` §6 |

## Instance types

| Method | Path | Description |
|---|---|---|
| GET | `/instance-types` | List presets (read-only in v1), smallest first |

Seed set: `kumo.nano` (1 vCPU/512 MiB), `kumo.micro` (1/1 GiB), `kumo.small` (1/2 GiB), `kumo.medium` (2/4 GiB), `kumo.large` (2/8 GiB), `kumo.xlarge` (4/16 GiB), `kumo.2xlarge` (8/32 GiB).

Seeding is idempotent (`INSERT OR IGNORE`): a preset edited on the node survives a restart.

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
| GET | `/images` | List |
| POST | `/images/import` | Import from URL (async, `202`) |
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
| `DELETE` | `pending`, `running`, `stopping`, `stopped`, `terminating`, `error` | `terminating` |

A `stop` that leaves a guest which ignores its ACPI request times out and leaves the row in
`stopping`; `stopping` is therefore also a state a stop may be asked from, so `?force=true` is the
way out. `DELETE` is answered without a task for an instance that is already `terminated`, so the
call is safe to repeat when a client never saw the first answer. `DELETE` keeps the row with
`terminated_at` set (see `02-data-model.md`), so the name becomes reusable and the launch inputs
stay for history.

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
