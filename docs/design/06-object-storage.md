# 06 — Object storage (S3-compatible, RustFS)

Kumo's first service beyond compute. **RustFS** stores the data and is the S3
endpoint applications talk to. **Kumo** is the control plane: buckets, the object
browser and access keys in the console, under Kumo's sign-in and roles. This is
the "modules registered under `/api/v1/<service>`" extension point of
`00-architecture.md` §10.

Approved as milestone M6 (`05-roadmap.md`), ahead of the open M5 items.

## 1. Shape

```
 console ──/api/v1/s3/*──▶ kumod ── service/bucket, service/s3key
                                      │
                                      ├─ store (SQLite: bucket metadata, tags, key records — never secrets)
                                      └─ objectstore.ObjectStore (interface)
                                           ├─ rustfs  (aws-sdk-go-v2, path-style, SigV4)
                                           └─ fake    (in memory; tests and KUMO_OBJECTSTORE=fake)

 console ──presigned PUT/GET──▶ RustFS (S3 port)      object bytes never pass through kumod
 apps    ──S3 API (SigV4)────▶ RustFS (S3 port)      with access keys Kumo issued
```

- **Layering is unchanged:** handlers call the services, and the services call
  `internal/objectstore`. Nothing outside `internal/objectstore/rustfs` imports the
  AWS SDK.
- **Two sources of truth, like libvirt and SQLite:**
  - RustFS is the truth for which buckets exist and what they hold.
  - SQLite holds what Kumo adds: who created a bucket and its tags. The list of
    buckets is always read from RustFS.
  - A bucket created outside Kumo still appears. Kumo manages **everything** in the
    RustFS it is pointed at.
- **Data path: presigned URLs.**
  - For an upload or download, the console asks kumod for a URL signed with Kumo's
    keys, valid for `KUMO_S3_PRESIGN_TTL`.
  - The browser then talks to RustFS directly, so RustFS must be reachable from
    users' browsers at `KUMO_S3_PUBLIC_URL`.
  - A SigV4 signature covers the host, so the URL is signed for the public address
    and not for `KUMO_S3_ENDPOINT`.
- **Long operations are tasks.** Bucket create, versioning and CORS changes are a
  single fast request each and stay synchronous. Emptying a bucket before a
  forced delete can take minutes, so it is a task (`bucket.delete`).

## 2. Configuration

The module is **optional and off by default**. With `KUMO_OBJECTSTORE=none`, the
`/api/v1/s3/*` routes answer `503 unavailable`, `/system/info` reports
`object_storage.backend: "none"` and the console hides the Storage pages. With the
module off, the `KUMO_S3_*` variables are not validated, so a half-written block in
`kumod.env` does not stop a compute-only node.

| Variable | Default | Description |
|---|---|---|
| `KUMO_OBJECTSTORE` | `none` | `none`, `rustfs` or `fake` |
| `KUMO_S3_ENDPOINT` | *(required with `rustfs`)* | where kumod reaches the S3 API, e.g. `http://127.0.0.1:9000` |
| `KUMO_S3_PUBLIC_URL` | `$KUMO_S3_ENDPOINT` | where browsers reach it; presigned URLs are signed for this host |
| `KUMO_S3_REGION` | `us-east-1` | signed into every request; RustFS accepts any value |
| `KUMO_S3_CREDENTIALS_FILE` | `$KUMO_DATA_DIR/s3.credentials` | RustFS admin keys; must not be readable by group or others |
| `KUMO_S3_CORS_ORIGINS` | *(empty)* | comma-separated console origins allowed to use presigned URLs; empty means browsers refuse console uploads |
| `KUMO_S3_PRESIGN_TTL` | `15m` | lifetime of a presigned URL, 1s–168h (the SigV4 ceiling) |

**Credentials file:**
- Format: `KEY=value` lines, with `#` comments and optional quotes. It takes
  `access_key`/`secret_key`, or `RUSTFS_ACCESS_KEY`/`RUSTFS_SECRET_KEY`, so an
  operator can copy `/etc/default/rustfs`.
- kumod **refuses a file group or others can read**, the way ssh refuses a private
  key: these keys own every bucket.
- The keys are held in memory and never logged. Error messages name the file,
  never a value from it.

**Readiness:**
- A RustFS that is down does **not** fail `/readyz` or stop kumod from starting.
  Compute does not depend on RustFS, and an optional module should not take the
  node out of a load balancer.
- `/system/info` reports `object_storage.ok`, and the console shows the Storage
  pages as unavailable while it is false.
- `kumod doctor` **fails** an unreachable server or an unsafe credentials file,
  because the operator asked for the module. It warns when
  `KUMO_S3_CORS_ORIGINS` is empty.

## 3. The `objectstore` interface

`internal/objectstore` defines `ObjectStore`:
- `Ping`
- buckets: `ListBuckets`, `HeadBucket`, `CreateBucket`, `DeleteBucket`,
  `EmptyBucket`, `GetVersioning`, `SetVersioning`, `SetCORS`
- objects: `ListObjects`, `HeadObject`, `PutObject`, `DeleteObjects`
- presigning: `PresignGet`, `PresignPut`

Errors are the sentinels `ErrNotFound`, `ErrAlreadyExists`, `ErrBucketNotEmpty`,
`ErrInvalidName` and `ErrUnavailable`. The services map them to
`404`/`409`/`409`/`400`/`503`. Access keys (RustFS's admin API) join the interface
in the access keys PR.

**Behaviour verified against RustFS 1.0.4.** `internal/objectstore/storetest` is
one suite run by the fake on every `go test`, and by the RustFS client under
`make test-rustfs`. What it pins down:

- **Creating a bucket that is already yours succeeds** (the AWS us-east-1 rule).
  The client looks the bucket up first, so `CreateBucket` reports
  `ErrAlreadyExists` either way.
- **Once versioning has been on, deleting objects leaves old versions and delete
  markers behind**, and `DeleteBucket` refuses the bucket. `EmptyBucket` lists
  versions, not objects, and deletes every version and marker. A bucket that was
  never versioned goes down the same path.
- **RustFS adds no CORS headers of its own.** A preflight to a bucket with no
  rules returns `200` with no `Access-Control-Allow-Origin`, and the browser then
  refuses the upload. Kumo therefore keeps **one** rule per bucket, with ID
  `kumo-console`, allowing `KUMO_S3_CORS_ORIGINS` to `GET`/`PUT`/`HEAD`.
  `SetCORS` replaces that rule and keeps any rule an operator wrote. Kumo sets
  the rule when it creates a bucket, and the first time it presigns for a bucket
  that lacks it.
- **Presigned URLs work from a plain HTTP client:** a `PUT` with the signed
  `Content-Type`, then a `GET`. A `PUT` with a different `Content-Type` is `403`.
- **Checksums:** since early 2025, aws-sdk-go-v2 adds CRC32 trailers to every
  upload by default. These are refused by some S3-compatible servers and would be
  signed into presigned URLs a browser cannot satisfy. The client sets
  `RequestChecksumCalculation` and `ResponseChecksumValidation` to `WhenRequired`,
  and a unit test checks no presigned URL carries a checksum parameter.

## 4. API (`/api/v1/s3`)

Reads are open to both roles; every write is `admin`. The endpoints arrive with
their PRs and are specified in `01-api.md` and `api/openapi.yaml` as they land;
the bucket and object endpoints are there now.

| Method | Path | Notes |
|---|---|---|
| GET | `/s3/buckets` | list from RustFS joined with Kumo's metadata |
| POST | `/s3/buckets` | `{name, versioning?, tags?}` → `201`; the name follows the S3 rules (3–63 chars, lowercase letters, digits, `-` and `.`, no IP-address form) |
| GET | `/s3/buckets/{name}` | detail: versioning, the endpoint and the region |
| PATCH | `/s3/buckets/{name}` | `{versioning?, tags?}` |
| DELETE | `/s3/buckets/{name}` | `204` when empty, `409` otherwise; `?force=true` → `202` with a `bucket.delete` task that empties it first |
| GET | `/s3/buckets/{name}/objects` | `?prefix=&cursor=&limit=`; folders are common prefixes on `/` |
| POST | `/s3/buckets/{name}/uploads` | `{key, content_type}` → presigned `PUT` |
| POST | `/s3/buckets/{name}/downloads` | `{key}` → presigned `GET` that saves under the key's base name |
| POST | `/s3/buckets/{name}/folders` | `{prefix}` → a zero-byte `prefix/` marker |
| POST | `/s3/buckets/{name}/objects/delete` | `{keys: [...]}`, at most 1000; a folder key removes only its marker, never what is below it |
| GET/POST/DELETE | `/s3/access-keys` | RustFS service accounts with a policy scoped to chosen buckets, read-only or read-write; the secret is returned **once** and never stored or logged |

Upload size: a single presigned `PUT` carries up to 5 GiB, the S3 limit.
Multipart upload from the browser is out of scope for M6.

## 5. Access keys

Each Kumo access key is a **RustFS user with a policy of its own**: the level's bucket actions on each bucket, and its object actions on everything inside. Permission checks run on RustFS's core IAM path, and the keys appear in the RustFS console.

**Calls:** creating a key is three admin calls: `add-canned-policy`, then `add-user`, then `set-user-or-group-policy`. A failure in any of them undoes the earlier ones. Deleting removes the user first, so the key stops working even if the policy removal then fails.

**Admin body encryption:** the admin API encrypts the bodies that carry credentials (the new user's secret, the user list) with the admin secret key, in madmin's format:
- a 32-byte salt, an algorithm byte, a nonce, then a sio (DARE) stream;
- RustFS 1.0.4 uses algorithm `0x02`, PBKDF2-SHA256 (8192 rounds) with AES-256-GCM.

Kumo implements this in `internal/objectstore/rustfs/crypto.go`, with `github.com/secure-io/sio-go` (MIT) and Go's own `crypto/pbkdf2`. MinIO's `madmin-go` is not used because it is AGPL-3.0.

**Verified against RustFS 1.0.4** by `TestAccessKeyIsEnforced` (`make test-rustfs`):
- A read-only key reads and lists its bucket. It cannot write there or read another bucket.
- Re-scoped to read-write, it writes and deletes objects in its bucket. It cannot write another bucket, delete its bucket, or create a bucket.
- A deleted key is refused.

## 6. Security

- The RustFS admin keys stay in kumod. The browser only ever holds a presigned URL
  for one key, one method and a few minutes.
- Access key secrets follow the private key rule: shown once in the create
  response, then gone.
- **Plain HTTP.** Kumo has no TLS yet (an M5 item), so on a non-loopback address
  a presigned URL, and an access key secret on its one display, cross the network
  in clear. The console warns when it is served over plain HTTP. This was accepted
  for M6, with TLS still tracked in M5.
- CORS origins are configuration, not request data: kumod never echoes a
  request's `Origin` into a bucket rule.
