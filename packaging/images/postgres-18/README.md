# Postgres 18 golden image

The recipe for the image a Kumo DB instance boots from. Built **once, outside
Kumo**, published as a qcow2 with its SHA256, and imported into a node like any
other image. See [`docs/design/07-databases.md`](../../../docs/design/07-databases.md)
§7 for why it works this way.

Nothing in `kumod` or the `Makefile` depends on this directory. A Kumo host
needs no image-building tooling at all; this runs on whatever machine publishes
images.

## Requirements

`qemu-system-x86_64`, `qemu-img`, `xorriso`, `curl`, `sha256sum`, and write
access to `/dev/kvm`. `gpg` is optional and used to check the signature over the
base image's checksums. About 1.2 GiB of downloads and 10 minutes with KVM;
`--no-kvm` works and takes roughly an hour.

## Build

```sh
./build.sh                      # writes out/kumo-postgres-18-<UTC date>.qcow2
./verify.sh --image out/kumo-postgres-18-<date>.qcow2
```

`build.sh` fetches the Ubuntu 24.04 cloud image, verifies its SHA256 (and the
signature over the checksum file against the pinned Ubuntu Cloud Image key
`D2EB 4462 6FDD C30B 513D 5BB7 1A5D 6C4C 7DB8 7C81`), boots it under QEMU with
`build.cloud-config.yaml` as its cloud-init seed, and waits for the guest to
provision itself and power off. It then flattens the result and writes three
files beside it:

| File | Contents |
|---|---|
| `*.qcow2` | the image, uncompressed and sparse |
| `*.sha256` | `sha256sum` line, for publishing |
| `*.manifest` | the engine's release and package version, the base image and its SHA256, the size and virtual size, the `min_disk_gb` to import with, the build time |

The output is **uncompressed on purpose.** It becomes the read-only backing file
of every DB instance's root overlay, so a compressed qcow2 would trade a smaller
download for decompressing every cold read a database makes for as long as the
instance lives.

Two builds are not byte-identical: the recipe takes current security updates
(`package_upgrade: true`), which is the right trade for an image that runs a
database. The manifest records what a given build actually contains.

## What the image contains

- Ubuntu 24.04, Postgres 18 from the PostgreSQL Debian repository. The repo's
  signing key is pinned here as `pgdg-archive-keyring.asc` (fingerprint
  `B97B 0AFC AA1A 47F0 44F2 44A0 7FCC 7D46 ACCC 4CF8`) and installed from this
  file rather than fetched mid-build.
- **No cluster.** The packaging runs `initdb` on install; the build drops that
  cluster. First boot creates one on the data disk, so no two DB instances share
  a cluster identity and no data ever lands on the root overlay.
- **No identity**: empty `/etc/machine-id`, no SSH host keys, no cloud-init
  state. systemd and cloud-init regenerate theirs on first boot.
- **No credentials**: no authorized keys, no passwords, `PasswordAuthentication
  no`. A DB instance is not a machine anyone has a shell on.
- `/etc/kumo-image` describes the image for whoever inspects one later —
  including `conf_include`, the file first boot writes the memory settings into.
  Nothing reads it; `kumod` cannot see inside a guest.

## What first boot does

`verify.cloud-config.yaml` is a hand-written stand-in for the seed
`internal/service/dbcloudinit` will render (M7.4). It follows
`07-databases.md` §5 step for step and is the executable record of what the
image expects at launch:

1. format the data disk if it has no filesystem, label `pgdata`
2. mount it at `/var/lib/postgresql`, by label, with an `fstab` entry
3. `pg_createcluster 18 main` on it, UTF-8 / `C.UTF-8`, `scram-sha-256`
4. write `conf.d/10-kumo.conf` — `listen_addresses` still `localhost`
5. write `pg_hba.conf` admitting the VPC CIDR and nothing else
6. start, then create the admin role from a **SCRAM verifier** and its database
7. set `listen_addresses = '*'` and restart — **last**
8. shred cloud-init's copies of the seed

Two details the implementation must keep:

- **The data disk is found by serial** (`/dev/disk/by-id/virtio-kumodata`), not
  by enumeration order, so the domain XML has to set `<serial>kumodata</serial>`
  on it. A boot that formats the wrong disk is not a bug anyone wants twice.
- **Step 7 is last.** Until that restart nothing outside the guest can connect,
  so the port being open means setup finished — which is what lets kumod's
  readiness probe be a bare TCP connect with no Postgres driver in the binary.

## Verify

`verify.sh` boots the image the way Kumo will — an overlay as the root disk, a
blank second disk, the seed above — and checks the four things that have to hold:
the cluster lands on the data disk and not the overlay; a role created from a
pre-computed SCRAM-SHA-256 verifier authenticates the original cleartext
password; the port is reachable from outside the guest; and booting does not
modify the published image.

The verifier in `verify.cloud-config.yaml` is a fixture with a fixed salt, for
the password `kumo-verify-pw-2026`. It is a test credential for a VM that is
deleted at the end of the run, and the image carries no trace of it. The real
thing is `internal/service/dbcred` (M7.4), with a random salt per DB instance.

## Publish

Host the `.qcow2` and its `.sha256` wherever Kumo nodes can reach them over
HTTPS, then import:

```sh
curl -sS -X POST https://kumo.example/api/v1/images/import \
  -H 'Authorization: Bearer <admin jwt>' \
  -H 'content-type: application/json' \
  -d '{
    "name": "postgres-18",
    "source_url": "https://.../kumo-postgres-18-<date>.qcow2",
    "checksum": {"algorithm": "sha256", "value": "<from the .sha256 file>"},
    "min_disk_gb": 6,
    "kind": "engine", "engine": "postgres", "engine_version": "18"
  }' | jq .
```

The checksum is a nested object, not two flat fields: an unknown field is
ignored, so a body with `checksum_algo`/`checksum_value` imports **without
verifying anything**. `docs/install.md` has the same call with the login in
front of it.

`kind=engine` keeps it off the Images pages and makes it eligible for
`POST /rds/db-instances` (M7.2). Use the `min_disk_gb` the manifest reports: a
root overlay smaller than the image's virtual size cannot boot.

A published image is immutable. A patched Postgres 18 is a **new** image
imported alongside the old one: existing DB instances keep the backing file they
booted from, and new ones get the newest available.
