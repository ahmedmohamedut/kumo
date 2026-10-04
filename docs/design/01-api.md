# 01 — REST API (v1)

Base path: `/api/v1`. JSON only. Auth: `Authorization: Bearer <access_jwt>` except where noted.
Source of truth for schemas is `api/openapi.yaml` (to be written in M0 from this doc).

## Conventions

- Resource IDs: `i-…`, `img-…`, `key-…`, `task-…`.
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
| GET | `/instances` | List (filter `?state=running&q=web`) |
| POST | `/instances` | Launch (async, `202`) |
| GET | `/instances/{id}` | Detail |
| PATCH | `/instances/{id}` | Update `name`, `tags` |
| POST | `/instances/{id}/start` | `202` if `stopped` |
| POST | `/instances/{id}/stop` | ACPI graceful shutdown; `?force=true` → destroy |
| POST | `/instances/{id}/reboot` | ACPI reboot; `?force=true` → reset |
| DELETE | `/instances/{id}` | Terminate: destroy domain, delete disks. `202` |
| GET | `/instances/{id}/console-output` | Last N KiB of serial console log |
| GET | `/instances/{id}/console` | WebSocket → VNC proxy (*M5*) |

Launch request:
```json
{ "name": "web-1", "image_id": "img-…", "instance_type": "kumo.small",
  "key_pair_id": "key-…", "root_disk_gb": 20,
  "user_data": "#cloud-config\npackages: [nginx]\n",
  "count": 1, "tags": {"env":"dev"} }
```
Validation: `name` 1–63 chars RFC-1123 label-ish; `root_disk_gb` ≥ image `min_disk_gb` (default: the image's virtual size rounded up), ≤ 2048; `user_data` ≤ 16 KiB; `count` 1–10 (MVP: 1 is acceptable); image must be `available`; host capacity check (`quota_exceeded`).

Instance resource:
```json
{ "id": "i-…", "name": "web-1", "state": "running", "state_reason": null,
  "image_id": "img-…", "instance_type": "kumo.small", "vcpus": 1, "memory_mib": 2048,
  "root_disk_gb": 20, "key_pair_id": "key-…", "private_ip": "192.168.100.23",
  "mac_address": "52:54:00:…", "launched_at": "…", "tags": {}, "task_id": "task-…" }
```
`user_data` is never returned by list; detail returns it only to admins.

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
