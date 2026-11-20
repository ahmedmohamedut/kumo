#!/usr/bin/env bash
# verify.sh -- check a built Kumo Spark 4.3 golden image before publishing it.
#
# Boots the image the way Kumo will -- a qcow2 overlay on top of it as the root
# disk and a NoCloud seed (verify.cloud-config.yaml, a stand-in for the seeds
# internal/service/sparkcloudinit will render, docs/design/09-spark.md
# section 4) -- as a master, a worker and a history server in one VM, and
# checks what the image is supposed to make true:
#   - the master answers and the worker registers ALIVE
#   - SparkPi (the JVM path) and a PySpark job (the Python path) run, with
#     authentication and encryption on
#   - a client with the wrong secret is refused
#   - nothing listens on the REST submission port
#   - the history server lists both jobs from their event logs
#   - the published image file is not modified by booting from it
# Every check runs in the guest and reports on the serial console.
#
#   ./verify.sh --image out/kumo-spark-4.3-20261121.qcow2
#
# Options:
#   --image FILE     the image to verify (required)
#   --work-dir DIR   scratch space (default: /var/tmp/kumo-image-build/verify)
#   --timeout SECS   how long the guest gets (default: 900)
#   --no-kvm         software emulation
#   --keep           keep the disks and the serial log
#   --help
set -euo pipefail
export LC_ALL=C

RECIPE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

IMAGE=
WORK_DIR=${KUMO_IMAGE_WORK_DIR:-/var/tmp/kumo-image-build}/verify
TIMEOUT=900
USE_KVM=1
KEEP=0
QEMU_PID=

say() { printf '%s\n' "$*"; }
err() { printf 'verify.sh: %s\n' "$*" >&2; }
die() {
	err "$*"
	exit 1
}

usage() {
	sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//; $d'
	exit 0
}

while [ $# -gt 0 ]; do
	case $1 in
	--image)
		IMAGE=$2
		shift 2
		;;
	--work-dir)
		WORK_DIR=$2
		shift 2
		;;
	--timeout)
		TIMEOUT=$2
		shift 2
		;;
	--no-kvm)
		USE_KVM=0
		shift
		;;
	--keep)
		KEEP=1
		shift
		;;
	--help | -h) usage ;;
	*) die "unknown option: $1 (try --help)" ;;
	esac
done

[ -n "$IMAGE" ] || die "--image is required (try --help)"
[ -f "$IMAGE" ] || die "no such image: $IMAGE"
IMAGE=$(cd -- "$(dirname -- "$IMAGE")" && pwd)/$(basename -- "$IMAGE")

cleanup() {
	[ -n "$QEMU_PID" ] && kill "$QEMU_PID" 2>/dev/null || true
	wait "$QEMU_PID" 2>/dev/null || true
}
trap cleanup EXIT

SERIAL=$WORK_DIR/verify-serial.log
ROOT=$WORK_DIR/root.qcow2

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR/seed"

# The root disk is an overlay on the image, as hypervisor.CreateRootDisk makes
# one, so booting exercises the thing every node depends on.
IMAGE_SHA_BEFORE=$(sha256sum "$IMAGE" | cut -d' ' -f1)
qemu-img create -q -f qcow2 -F qcow2 -b "$IMAGE" "$ROOT" 10G

cp "$RECIPE_DIR/verify.cloud-config.yaml" "$WORK_DIR/seed/user-data"
cat >"$WORK_DIR/seed/meta-data" <<-META
	instance-id: kumo-verify-$(date -u +%s)
	local-hostname: spark-verify
META
xorriso -as mkisofs -quiet -V cidata -J -r -o "$WORK_DIR/seed.iso" "$WORK_DIR/seed"

accel=()
[ "$USE_KVM" -eq 1 ] && accel=(-enable-kvm -cpu host)

# A master, a worker with 2 GiB for executors, a driver and the history server
# in one guest: 6 GiB. The user network routes nothing the checks need; every
# Spark connection stays inside the guest.
say "==> booting $(basename "$IMAGE") (serial log: $SERIAL)"
qemu-system-x86_64 \
	"${accel[@]}" \
	-m 6144 -smp 4 \
	-display none -monitor none -no-reboot \
	-serial "file:$SERIAL" \
	-drive "if=none,id=root,file=$ROOT,format=qcow2" \
	-device "virtio-blk-pci,drive=root,bootindex=0" \
	-drive "if=none,id=seed,file=$WORK_DIR/seed.iso,format=raw,readonly=on" \
	-device "virtio-blk-pci,drive=seed" \
	-netdev "user,id=n0,restrict=on" \
	-device virtio-net-pci,netdev=n0 2>"$WORK_DIR/qemu.stderr" &
QEMU_PID=$!

waited=0
while :; do
	grep -q 'KUMO-SPARK-READY' "$SERIAL" 2>/dev/null && break
	if grep -q 'KUMO-SPARK-FAILED' "$SERIAL" 2>/dev/null; then
		tail -40 "$SERIAL" >&2
		die "a check failed in the guest; see $SERIAL"
	fi
	if ! kill -0 "$QEMU_PID" 2>/dev/null; then
		tail -40 "$SERIAL" >&2
		die "the guest died before reporting; see $SERIAL"
	fi
	[ "$waited" -lt "$TIMEOUT" ] || {
		tail -40 "$SERIAL" >&2
		die "the checks did not finish within ${TIMEOUT}s; see $SERIAL"
	}
	sleep 3
	waited=$((waited + 3))
done

marker() { sed -n "s/.*$1=\([^ \r]*\).*/\1/p" "$SERIAL" | tail -1; }

IMAGE_SHA_AFTER=$(sha256sum "$IMAGE" | cut -d' ' -f1)
cleanup
QEMU_PID=

say ""
say "master            $(marker KUMO-SPARK-MASTER)"
say "SparkPi           $(marker KUMO-SPARK-PI)"
say "PySpark sum       $(marker KUMO-SPARK-PYSPARK)"
say "wrong secret      $(marker KUMO-SPARK-WRONG-SECRET)"
say "  because         $(marker KUMO-SPARK-WRONG-SECRET-WHY | tr '_' ' ')"
say "REST server       $(marker KUMO-SPARK-REST)"
say "history server    $(marker KUMO-SPARK-HISTORY) applications"
say "image unchanged   $([ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] && echo yes || echo NO)"
say ""

rc=0
[ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] || {
	err "booting modified the published image"
	rc=1
}
[ "$KEEP" -eq 1 ] || rm -rf "$WORK_DIR"
[ "$rc" -eq 0 ] && say "VERIFIED" || err "verification failed"
exit "$rc"
