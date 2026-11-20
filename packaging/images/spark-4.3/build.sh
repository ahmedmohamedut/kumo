#!/usr/bin/env bash
# build.sh -- build the Kumo Spark 4.3 golden image.
#
# A wrapper: the build itself is ../lib/build-image.sh, shared by every engine
# image recipe, and this directory supplies recipe.env, the build seed and the
# files/ the seed installs.
#
#   ./build.sh                       # ~1.2 GiB of downloads, about 15 minutes
#   ./build.sh --out /tmp/spark-43.qcow2
#
# Every option of build-image.sh except --recipe applies (./build.sh --help).
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
exec "$here/../lib/build-image.sh" --recipe "$here" "$@"
