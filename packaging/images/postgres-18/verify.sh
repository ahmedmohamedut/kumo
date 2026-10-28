#!/usr/bin/env bash
# verify.sh -- check a built Kumo Postgres 18 golden image before publishing it.
#
# Boots the image the way Kumo will: a qcow2 overlay on top of it as the root
# disk, a blank second disk as the data disk, and a NoCloud seed standing in for
# the one internal/service/dbcloudinit will render
# (docs/design/07-databases.md §5, verify.cloud-config.yaml here).
#
# It then checks the things the image is supposed to make true:
#   - first boot formats the blank disk and creates the cluster ON IT, not on
#     the root overlay (R4)
#   - a role created from a pre-computed SCRAM-SHA-256 verifier authenticates
#     the original cleartext password (R8, and the format M7.4 must produce)
#   - the port is reachable from outside the guest only once setup finished,
#     which is what makes kumod's bare-TCP readiness probe truthful (§6)
#   - the published image file is not modified by booting from it
#
#   ./verify.sh --image out/kumo-postgres-18-20261028.qcow2
#
# Options:
#   --image FILE     the image to verify (required)
#   --work-dir DIR   scratch space (default: /var/tmp/kumo-image-build/verify)
#   --port PORT      host port forwarded to the guest's 5432 (default: 15432)
#   --timeout SECS   how long first boot gets (default: 600)
#   --no-kvm         software emulation
#   --keep           keep the disks and the serial log
#   --help
set -euo pipefail
export LC_ALL=C

RECIPE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

IMAGE=
WORK_DIR=${KUMO_IMAGE_WORK_DIR:-/var/tmp/kumo-image-build}/verify
PORT=15432
TIMEOUT=600
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
	--port)
		PORT=$2
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
DATA=$WORK_DIR/data.qcow2

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR/seed"

# The root disk is an overlay on the image, created exactly as
# hypervisor.CreateRootDisk will: a qcow2 with the image as its backing file,
# resized to the instance's root_disk_gb. Booting any other way would not
# exercise the thing every DB instance depends on.
IMAGE_SHA_BEFORE=$(sha256sum "$IMAGE" | cut -d' ' -f1)
qemu-img create -q -f qcow2 -F qcow2 -b "$IMAGE" "$ROOT" 10G
# A blank data disk, which is what step 1 of the first-boot script must format.
qemu-img create -q -f qcow2 "$DATA" 2G

cp "$RECIPE_DIR/verify.cloud-config.yaml" "$WORK_DIR/seed/user-data"
cat >"$WORK_DIR/seed/meta-data" <<-META
	instance-id: kumo-verify-$(date -u +%s)
	local-hostname: orders
META
xorriso -as mkisofs -quiet -V cidata -J -r -o "$WORK_DIR/seed.iso" "$WORK_DIR/seed"

accel=()
[ "$USE_KVM" -eq 1 ] && accel=(-enable-kvm -cpu host)

# Every disk is an explicit -device with the root disk's bootindex pinned. With
# the `-drive if=virtio` shorthand the firmware is free to try the blank data
# disk first and then stop looking, which presents as a VM that burns four
# seconds of CPU and hangs with an empty serial log, because the firmware's
# complaint goes to the VGA console that --display none discards.
say "==> booting $(basename "$IMAGE") (serial log: $SERIAL)"
qemu-system-x86_64 \
	"${accel[@]}" \
	-m 2048 -smp 2 \
	-display none -monitor none -no-reboot \
	-serial "file:$SERIAL" \
	-drive "if=none,id=root,file=$ROOT,format=qcow2" \
	-device "virtio-blk-pci,drive=root,bootindex=0" \
	-drive "if=none,id=kumodata,file=$DATA,format=qcow2,discard=unmap" \
	-device "virtio-blk-pci,drive=kumodata,serial=kumodata" \
	-drive "if=none,id=seed,file=$WORK_DIR/seed.iso,format=raw,readonly=on" \
	-device "virtio-blk-pci,drive=seed" \
	-netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$PORT-:5432" \
	-device virtio-net-pci,netdev=n0 2>"$WORK_DIR/qemu.stderr" &
QEMU_PID=$!

waited=0
while :; do
	if grep -q 'KUMO-DB-READY' "$SERIAL" 2>/dev/null; then
		break
	fi
	if grep -q 'KUMO-DB-FAILED' "$SERIAL" 2>/dev/null; then
		tail -40 "$SERIAL" >&2
		die "first boot reported a failure; see $SERIAL"
	fi
	if ! kill -0 "$QEMU_PID" 2>/dev/null; then
		tail -40 "$SERIAL" >&2
		die "the guest died before reporting readiness; see $SERIAL"
	fi
	[ "$waited" -lt "$TIMEOUT" ] || {
		tail -40 "$SERIAL" >&2
		die "first boot did not finish within ${TIMEOUT}s; see $SERIAL"
	}
	sleep 3
	waited=$((waited + 3))
done

marker() { sed -n "s/.*$1=\([^ \r]*\).*/\1/p" "$SERIAL" | tail -1; }

login=$(marker KUMO-DB-LOGIN)
datadir=$(marker KUMO-DB-DATADIR)
datadisk=$(marker KUMO-DB-DATADISK)
addr=$(marker KUMO-DB-ADDR)

# The probe kumod will use, from outside the guest: a bare TCP connect and
# nothing else. It is meaningful only because the first-boot script opens
# listen_addresses last, so reaching it here is the signal that setup finished.
probe=fail
if timeout 10 bash -c "exec 3<>/dev/tcp/127.0.0.1/$PORT" 2>/dev/null; then
	probe=ok
fi

IMAGE_SHA_AFTER=$(sha256sum "$IMAGE" | cut -d' ' -f1)

cleanup
QEMU_PID=

say ""
say "cluster datadir   $datadir (data disk: $datadisk)"
say "guest address     $addr"
say "SCRAM login       ${login:-FAILED}"
say "TCP probe :$PORT  $probe"
say "image unchanged   $([ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] && echo yes || echo NO)"
say ""

rc=0
[ -n "$login" ] || {
	err "the admin role did not authenticate with the original password"
	rc=1
}
[ "$probe" = ok ] || {
	err "the engine was not reachable from outside the guest"
	rc=1
}
[ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] || {
	err "booting modified the published image"
	rc=1
}
if [ -z "$datadir" ] || [ "$datadir" != "$datadisk" ]; then
	err "the cluster is on ${datadir:-unknown}, not on the data disk ${datadisk:-unknown}"
	rc=1
fi

[ "$KEEP" -eq 1 ] || rm -rf "$WORK_DIR"
[ "$rc" -eq 0 ] && say "VERIFIED" || err "verification failed"
exit "$rc"
