#!/usr/bin/env bash
# verify.sh -- check a built Kumo Kubernetes 1.37 image before publishing it.
#
# Boots the image twice, the way Kumo will: a qcow2 overlay on top of it as the
# root disk of a control plane and of a worker, each with a NoCloud seed
# standing in for the one kumod will render (verify-*.cloud-config.yaml).
#
# The two VMs share one private network and nothing else: a QEMU socket link
# between them, with no NAT, no gateway that exists and no DNS server that
# answers. So any image the cluster tries to pull fails, which is the proof
# that the image carries everything (docs/design/08-kubernetes.md §7).
#
# The control plane then checks, and reports on its serial console:
#   - kubeadm init succeeds and the worker joins; both nodes Ready
#   - Cilium, CoreDNS and local-path roll out from the image's own manifests
#   - a PersistentVolumeClaim binds on the default StorageClass and is written
#   - a pod on the control plane reaches a pod on the worker (the VXLAN tunnel)
#   - no image-pull event happened anywhere in the cluster
# and this script checks that booting did not modify the published image.
#
#   ./verify.sh --image out/kumo-kubernetes-1.37-20261120.qcow2
#
# Options:
#   --image FILE     the image to verify (required)
#   --work-dir DIR   scratch space (default: /var/tmp/kumo-image-build/verify-kubernetes)
#   --timeout SECS   how long the cluster gets to come up (default: 1500)
#   --no-kvm         software emulation (very slow)
#   --keep           keep the disks and the serial logs
#   --help
set -euo pipefail
export LC_ALL=C

RECIPE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

IMAGE=
WORK_DIR=${KUMO_IMAGE_WORK_DIR:-/var/tmp/kumo-image-build}/verify-kubernetes
TIMEOUT=1500
USE_KVM=1
KEEP=0
CP_PID=
W_PID=

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
	for pid in $CP_PID $W_PID; do
		kill "$pid" 2>/dev/null || true
		wait "$pid" 2>/dev/null || true
	done
}
trap cleanup EXIT

rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR"

IMAGE_SHA_BEFORE=$(sha256sum "$IMAGE" | cut -d' ' -f1)

# seed NAME HOSTNAME IP MAC USER_DATA: a NoCloud seed with a static address on
# the verify network, a default route through a gateway that does not exist,
# and a DNS server that does not answer -- CoreDNS needs an upstream to start,
# and gets one that resolves nothing.
seed() {
	local name=$1 host=$2 ip=$3 mac=$4 userdata=$5 dir=$WORK_DIR/$1-seed
	mkdir -p "$dir"
	cp "$userdata" "$dir/user-data"
	cat >"$dir/meta-data" <<-META
		instance-id: kumo-verify-$host-$(date -u +%s)
		local-hostname: $host
	META
	cat >"$dir/network-config" <<-NET
		version: 2
		ethernets:
		  kumo0:
		    match:
		      macaddress: "$mac"
		    set-name: kumo0
		    addresses: [$ip/24]
		    routes:
		      - to: default
		        via: 10.250.0.1
		    nameservers:
		      addresses: [10.250.0.1]
	NET
	xorriso -as mkisofs -quiet -V cidata -J -r -o "$WORK_DIR/$name-seed.iso" "$dir"
	# The root disk is an overlay on the image, as hypervisor.CreateRootDisk
	# makes it, sized like a small node.
	qemu-img create -q -f qcow2 -F qcow2 -b "$IMAGE" "$WORK_DIR/$name-root.qcow2" 20G
}

CP_MAC=52:54:00:4b:00:10
W_MAC=52:54:00:4b:00:11
seed cp cp1 10.250.0.10 "$CP_MAC" "$RECIPE_DIR/verify-controlplane.cloud-config.yaml"
seed w w1 10.250.0.11 "$W_MAC" "$RECIPE_DIR/verify-worker.cloud-config.yaml"

accel=()
[ "$USE_KVM" -eq 1 ] && accel=(-enable-kvm -cpu host)

# boot NAME MEMORY MAC NETDEV: one VM, its only NIC on the socket link.
boot() {
	local name=$1 mem=$2 mac=$3 netdev=$4
	qemu-system-x86_64 \
		"${accel[@]}" \
		-m "$mem" -smp 2 \
		-display none -monitor none -no-reboot \
		-serial "file:$WORK_DIR/$name-serial.log" \
		-drive "if=none,id=root,file=$WORK_DIR/$name-root.qcow2,format=qcow2" \
		-device "virtio-blk-pci,drive=root,bootindex=0" \
		-drive "if=none,id=seed,file=$WORK_DIR/$name-seed.iso,format=raw,readonly=on" \
		-device "virtio-blk-pci,drive=seed" \
		-netdev "$netdev" \
		-device "virtio-net-pci,netdev=k0,mac=$mac" 2>"$WORK_DIR/$name-qemu.stderr" &
}

# A point-to-point link: the control plane listens on a loopback port and the
# worker connects to it. Neither VM has any other network.
PORT=$((20000 + RANDOM % 20000))
say "==> booting the control plane and the worker (serial logs in $WORK_DIR)"
boot cp 3072 "$CP_MAC" "socket,id=k0,listen=127.0.0.1:$PORT"
CP_PID=$!
sleep 2
boot w 2048 "$W_MAC" "socket,id=k0,connect=127.0.0.1:$PORT"
W_PID=$!

CP_LOG=$WORK_DIR/cp-serial.log
W_LOG=$WORK_DIR/w-serial.log
waited=0
while :; do
	grep -aq 'KUMO-K8S-READY' "$CP_LOG" 2>/dev/null && break
	if grep -aq 'KUMO-K8S-FAILED' "$CP_LOG" 2>/dev/null; then
		sed -n '/--- kumo-verify diagnostics/,$p' "$CP_LOG" | tail -80 >&2
		die "the control plane reported a failure; see $CP_LOG"
	fi
	if grep -aq 'KUMO-K8S-JOIN-FAILED' "$W_LOG" 2>/dev/null; then
		tail -40 "$W_LOG" >&2
		die "the worker could not join; see $W_LOG"
	fi
	for pid in $CP_PID $W_PID; do
		kill -0 "$pid" 2>/dev/null || die "a VM died before the cluster came up; see $WORK_DIR/*-qemu.stderr and the serial logs"
	done
	[ "$waited" -lt "$TIMEOUT" ] || {
		tail -30 "$CP_LOG" >&2
		die "the cluster did not come up within ${TIMEOUT}s; see $CP_LOG and $W_LOG"
	}
	sleep 5
	waited=$((waited + 5))
done

marker() { sed -n "s/.*$1=\([^\r]*\).*/\1/p" "$CP_LOG" | tail -1; }

cleanup
CP_PID= W_PID=
IMAGE_SHA_AFTER=$(sha256sum "$IMAGE" | cut -d' ' -f1)

say ""
say "kubeadm API       $(marker KUMO-K8S-KUBEADM-API)"
say "version           $(marker KUMO-K8S-VERSION)"
say "nodes             $(marker KUMO-K8S-NODES)"
say "add-ons           $(marker KUMO-K8S-ADDONS)"
say "volume claim      $(marker KUMO-K8S-PVC)"
say "pod network       $(marker KUMO-K8S-OVERLAY)"
say "image pulls       $(marker KUMO-K8S-PULLS)"
say "up after          ${waited}s"
say "image unchanged   $([ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] && echo yes || echo NO)"
say ""

[ "$IMAGE_SHA_BEFORE" = "$IMAGE_SHA_AFTER" ] || die "booting modified the published image"
[ "$KEEP" -eq 1 ] || rm -rf "$WORK_DIR"
say "VERIFIED"
