#!/usr/bin/env bash
# build.sh -- build the Kumo Postgres 18 golden image.
#
# A wrapper: the build itself is ../lib/build-image.sh, shared by every engine
# image recipe, and this directory supplies recipe.env and the build seed.
#
#   ./build.sh                       # ~1.2 GiB of downloads, about 10 minutes
#   ./build.sh --out /tmp/pg18.qcow2
#
# Every option of build-image.sh except --recipe applies (./build.sh --help).
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec "$here/../lib/build-image.sh" --recipe "$here" "$@"
