# Engine images

The golden images Kumo's managed modules boot from, one recipe per directory:

| Recipe | Module | Base | Design |
|---|---|---|---|
| [`postgres-18/`](postgres-18/) | Databases | Ubuntu 24.04 | [`07-databases.md`](../../docs/design/07-databases.md) §7 |
| [`kubernetes-1.37/`](kubernetes-1.37/) | Kubernetes clusters | Ubuntu 26.04 | [`08-kubernetes.md`](../../docs/design/08-kubernetes.md) §7 |
| [`spark-4.3/`](spark-4.3/) | Spark clusters | Ubuntu 26.04 | [`09-spark.md`](../../docs/design/09-spark.md) §7 |

Each is built **once, outside Kumo**, published with its SHA256, and imported
into a node like any other image with `kind=engine`. Nothing in `kumod` or the
`Makefile` depends on this directory, and a Kumo host needs no image-building
tooling at all.

## How a recipe is built

[`lib/`](lib/) is the build every recipe shares, so that what decides an image's
size and hygiene exists once:

- **`lib/build-image.sh`** runs on the build machine. It fetches the recipe's
  Ubuntu cloud image and checks it against `SHA256SUMS` and the signature over
  them (pinned Ubuntu Cloud Image key `D2EB 4462 6FDD C30B 513D 5BB7 1A5D 6C4C
  7DB8 7C81`), boots it under QEMU with the recipe's `build.cloud-config.yaml`,
  waits for the guest to provision itself and power off, then flattens the
  result and writes the `.qcow2`, its `.sha256` and a `.manifest`.
- **`lib/guest-finish.sh`** runs inside the guest as each recipe's last step:
  one kernel and no headers, no package caches, no machine identity or logs,
  freed blocks handed back, a size report in the serial log, then
  `KUMO-IMAGE-READY` and poweroff. It reaches the guest on a second, read-only
  ISO labelled `KUMOFILES`, along with the recipe's own `files/`.

A recipe directory holds `recipe.env` (name, engine, version, disk size, base
image), `build.cloud-config.yaml`, a `build.sh` that wraps `lib/build-image.sh`,
optionally `files/` and a `preflight.sh` run before any download, and its own
`verify.sh`, which boots the result the way Kumo will and checks what the image
has to make true.

Requirements on the build machine: `qemu-system-x86_64`, `qemu-img`, `xorriso`,
`curl`, `sha256sum`, write access to `/dev/kvm`, and `gpg` (optional for
Postgres, required for Kubernetes, whose preflight checks its pinned apt keys).

## Publish

The project's own images are built, verified and published by
[`.github/workflows/engine-images.yml`](../../.github/workflows/engine-images.yml),
for every recipe on the 2nd of each month and on demand (**Actions → Engine
images → Run workflow**, with one recipe or all, optionally with a version).
Each image that passes its `verify.sh` becomes a GitHub release tagged
`<recipe>-<version>` (`postgres-18-20261120`, `kubernetes-1.37-20261120`) with
the exact import body in its notes. A pull request that touches this directory
builds and verifies every recipe and publishes nothing.

What the workflow holds to:

- **Published gzipped, stored uncompressed.** The release carries
  `<image>.qcow2.gz`, its `.sha256` (of the `.gz`) and the `.manifest`, which
  records both checksums. `POST /images/import` decompresses a gzipped download
  after checking its checksum, so the pool holds the plain qcow2 every guest
  overlay is backed by. The gzip is transport only.
- **Under 2 GiB, gzipped.** GitHub refuses a release asset of 2 GiB or more.
  The workflow measures the `.gz`, warns from 95% of the limit and fails above
  it, before anything is uploaded.
- **Never marked latest.** `scripts/install.sh` resolves `/releases/latest` to
  find Kumo itself, so an image release must not become it. For the same reason
  the Makefile and the Kumo release job read versions with `--match 'v*'`.
- **The guest never sees a write token.** The jobs that boot qemu run with read
  access only and hand their image to a separate publish job, which checks each
  against its `.sha256` before creating the release.

A published image is immutable. A patched engine is a **new** image imported
alongside the old one.

## Adding a recipe

A new directory with the files above, its name added to the workflow's
`recipe` choices and to the `all` list in its plan job, and a row in the table
at the top of this file.
