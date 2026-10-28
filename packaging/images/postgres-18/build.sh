#!/usr/bin/env bash
# build.sh -- build the Kumo Postgres 18 golden image.
#
# Boots an Ubuntu 24.04 cloud image under QEMU with build.cloud-config.yaml as
# its cloud-init seed, lets it install Postgres 18 and strip its own identity,
# then compacts the result and records its SHA256 and a manifest.
#
# Run it on any machine with qemu and KVM. Deliberately NOT on a Kumo host as
# part of a release: nothing in kumod or the Makefile depends on this recipe,
# and the Kumo host needs no image-building tooling at all
# (docs/design/07-databases.md §7).
#
# The output is an uncompressed sparse qcow2, which is what Kumo imports and
# then uses as the read-only backing file of every DB instance's root overlay.
#
#   ./build.sh                       # ~1.2 GiB of downloads, about 10 minutes
#   ./build.sh --out /tmp/pg18.qcow2
#
# Options:
#   --out FILE        where to write the image (default: ./out/<name>.qcow2)
#   --version TAG     version string in the default output name (default: UTC date)
#   --work-dir DIR    base-image cache and scratch space (default: /var/tmp/kumo-image-build)
#   --disk-size SIZE  virtual size of the image, qemu-img syntax (default: 6G)
#   --base-url URL    directory holding the cloud image and its SHA256SUMS
#   --no-kvm          software emulation; an apt install this way takes about an hour
#   --keep            keep the build disk, the seed and the serial log on success
#   --help
set -euo pipefail
export LC_ALL=C

RECIPE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

BASE_URL=${KUMO_IMAGE_BASE_URL:-https://cloud-images.ubuntu.com/noble/current}
BASE_IMAGE=${KUMO_IMAGE_BASE_IMAGE:-noble-server-cloudimg-amd64.img}
WORK_DIR=${KUMO_IMAGE_WORK_DIR:-/var/tmp/kumo-image-build}
DISK_SIZE=6G
VERSION=$(date -u +%Y%m%d)
OUT=
USE_KVM=1
KEEP=0

# The Ubuntu Cloud Image signing key. Pinned by fingerprint so that fetching it
# from a keyserver is safe: the keyserver chooses nothing, it only hands over
# the key with this fingerprint or fails.
UBUNTU_KEY_FPR=D2EB44626FDDC30B513D5BB71A5D6C4C7DB87C81

# How long the guest gets to update, install Postgres and strip itself. Generous
# because it covers a slow mirror, and bounded because a build that is going to
# hang should fail the script rather than the person watching it.
BUILD_TIMEOUT=${KUMO_IMAGE_BUILD_TIMEOUT:-2700}

say() { printf '%s\n' "$*"; }
err() { printf 'build.sh: %s\n' "$*" >&2; }
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
	--out)
		OUT=$2
		shift 2
		;;
	--version)
		VERSION=$2
		shift 2
		;;
	--work-dir)
		WORK_DIR=$2
		shift 2
		;;
	--disk-size)
		DISK_SIZE=$2
		shift 2
		;;
	--base-url)
		BASE_URL=$2
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

: "${OUT:=$RECIPE_DIR/out/kumo-postgres-18-$VERSION.qcow2}"

# need_tools fails on the whole missing set at once rather than one per run.
need_tools() {
	local missing=()
	for t in qemu-system-x86_64 qemu-img xorriso curl sha256sum; do
		command -v "$t" >/dev/null 2>&1 || missing+=("$t")
	done
	[ ${#missing[@]} -eq 0 ] || die "missing required tools: ${missing[*]}"

	if [ "$USE_KVM" -eq 1 ] && [ ! -w /dev/kvm ]; then
		die "/dev/kvm is not writable by $(id -un); join the kvm group, or pass --no-kvm"
	fi
}

# fetch_base downloads the Ubuntu cloud image into the work directory and
# refuses to go on unless it matches the published checksum.
#
# The checksum is verified always. The signature over the checksum file is
# verified when gpg can be made to produce the pinned key, and skipped with a
# warning when it cannot: the sha256 already came over HTTPS from the same host,
# so the signature is defence against a compromised mirror rather than the only
# check. A signature that is present and *bad* is always fatal.
fetch_base() {
	mkdir -p "$WORK_DIR"
	local img="$WORK_DIR/$BASE_IMAGE"

	say "==> fetching $BASE_URL/$BASE_IMAGE"
	curl -fsSL --retry 3 -o "$WORK_DIR/SHA256SUMS" "$BASE_URL/SHA256SUMS"
	curl -fsSL --retry 3 -o "$WORK_DIR/SHA256SUMS.gpg" "$BASE_URL/SHA256SUMS.gpg" ||
		say "    note: no SHA256SUMS.gpg published at this base URL"

	if [ -s "$WORK_DIR/SHA256SUMS.gpg" ] && command -v gpg >/dev/null 2>&1; then
		local keyring=$WORK_DIR/ubuntu-cloud-image.gpg
		if [ ! -s "$keyring" ]; then
			rm -rf "$WORK_DIR/gnupg"
			mkdir -p "$WORK_DIR/gnupg"
			chmod 700 "$WORK_DIR/gnupg"
			if gpg --homedir "$WORK_DIR/gnupg" --batch --quiet \
				--keyserver hkps://keyserver.ubuntu.com \
				--recv-keys "$UBUNTU_KEY_FPR" 2>/dev/null; then
				gpg --homedir "$WORK_DIR/gnupg" --batch --export "$UBUNTU_KEY_FPR" >"$keyring"
			fi
		fi
		if [ -s "$keyring" ]; then
			gpgv --keyring "$keyring" "$WORK_DIR/SHA256SUMS.gpg" "$WORK_DIR/SHA256SUMS" >/dev/null 2>&1 ||
				die "SHA256SUMS signature does not verify against $UBUNTU_KEY_FPR"
			say "    signature verified ($UBUNTU_KEY_FPR)"
		else
			say "    warning: could not obtain $UBUNTU_KEY_FPR; checksum only"
		fi
	else
		say "    warning: gpg unavailable or no signature; checksum only"
	fi

	local want
	want=$(awk -v f="*$BASE_IMAGE" '$2 == f {print $1}' "$WORK_DIR/SHA256SUMS")
	[ -n "$want" ] || die "$BASE_IMAGE is not listed in SHA256SUMS"

	# Re-download only when the cached copy is absent or no longer matches, so
	# repeated builds cost nothing and a half-finished download cannot persist.
	if [ -f "$img" ] && [ "$(sha256sum "$img" | cut -d' ' -f1)" = "$want" ]; then
		say "    cached $img"
	else
		curl -fSL --retry 3 --progress-bar -o "$img.part" "$BASE_URL/$BASE_IMAGE"
		[ "$(sha256sum "$img.part" | cut -d' ' -f1)" = "$want" ] ||
			die "$BASE_IMAGE does not match its published sha256"
		mv "$img.part" "$img"
	fi
	BASE_SHA256=$want
}

# make_seed writes the NoCloud seed. cloud-init finds it by the filesystem
# label, so it is a plain virtio disk rather than a cdrom.
make_seed() {
	local dir=$WORK_DIR/seed
	rm -rf "$dir"
	mkdir -p "$dir"
	cp "$RECIPE_DIR/build.cloud-config.yaml" "$dir/user-data"
	cat >"$dir/meta-data" <<-META
		instance-id: kumo-image-build-$VERSION
		local-hostname: kumo-image-build
	META
	xorriso -as mkisofs -quiet -V cidata -J -r -o "$WORK_DIR/seed.iso" "$dir"
}

# run_build copies the base (rather than layering an overlay on it) so that the
# guest's discards genuinely unmap, which is what lets the convert at the end
# drop the blocks the build freed.
run_build() {
	local disk=$WORK_DIR/build.qcow2 serial=$WORK_DIR/build-serial.log
	rm -f "$disk" "$serial"
	qemu-img convert -O qcow2 "$WORK_DIR/$BASE_IMAGE" "$disk"
	qemu-img resize -q "$disk" "$DISK_SIZE"

	local accel=()
	[ "$USE_KVM" -eq 1 ] && accel=(-enable-kvm -cpu host)

	say "==> building (serial log: $serial)"
	local rc=0
	timeout "$BUILD_TIMEOUT" qemu-system-x86_64 \
		"${accel[@]}" \
		-m 2048 -smp 2 \
		-display none -monitor none -no-reboot \
		-serial "file:$serial" \
		-drive "if=virtio,file=$disk,format=qcow2,discard=unmap,cache=writeback" \
		-drive "if=virtio,file=$WORK_DIR/seed.iso,format=raw,readonly=on" \
		-netdev user,id=n0 -device virtio-net-pci,netdev=n0 || rc=$?

	if [ "$rc" -ne 0 ]; then
		err "qemu exited $rc (124 means it hit the ${BUILD_TIMEOUT}s timeout)"
		tail -40 "$serial" >&2 || true
		die "build did not finish"
	fi

	if grep -q 'KUMO-IMAGE-FAILED' "$serial"; then
		tail -40 "$serial" >&2
		die "the guest reported a failed provision; see $serial"
	fi
	grep -q 'KUMO-IMAGE-READY' "$serial" || {
		tail -40 "$serial" >&2
		die "the guest powered off without reporting success; see $serial"
	}
	ENGINE_RELEASE=$(sed -n 's/.*KUMO-IMAGE-ENGINE=\([0-9][^ \r]*\).*/\1/p' "$serial" | tail -1)
	ENGINE_PACKAGE=$(sed -n 's/.*KUMO-IMAGE-PKG=\([^ \r]*\).*/\1/p' "$serial" | tail -1)
	BUILD_DISK=$disk
	SERIAL_LOG=$serial
}

# finish flattens the build disk into the published image. Uncompressed on
# purpose: the file is the read-only backing store of every DB instance's root
# overlay, so a compressed qcow2 would trade a smaller download for
# decompressing every cold read a database makes for the life of the instance.
finish() {
	mkdir -p "$(dirname "$OUT")"
	say "==> compacting into $OUT"
	qemu-img convert -O qcow2 -o compat=1.1 "$BUILD_DISK" "$OUT.part"
	mv "$OUT.part" "$OUT"

	local sha size virt mindisk
	sha=$(sha256sum "$OUT" | cut -d' ' -f1)
	size=$(stat -c %s "$OUT")
	# From qemu-img's plain output, not its JSON: the JSON nests a `file` child
	# whose own "virtual-size" is the file length and comes first, so picking a
	# line out of it reports the size on disk as the guest capacity. The plain
	# output has one anchored line and cannot be confused that way.
	virt=$(qemu-img info "$OUT" | sed -n 's/^virtual size: .*(\([0-9]*\) bytes)/\1/p')
	[ -n "$virt" ] || die "could not read the virtual size of $OUT"
	# What to import the image with: a root overlay must be at least this big.
	mindisk=$(((virt + 1073741823) / 1073741824))

	printf '%s  %s\n' "$sha" "$(basename "$OUT")" >"$OUT.sha256"
	cat >"$OUT.manifest" <<-MANIFEST
		image=$(basename "$OUT")
		sha256=$sha
		size_bytes=$size
		virtual_size_bytes=$virt
		engine=postgres
		engine_version=18
		engine_release=${ENGINE_RELEASE:-unknown}
		engine_package=${ENGINE_PACKAGE:-unknown}
		min_disk_gb=$mindisk
		base_url=$BASE_URL/$BASE_IMAGE
		base_sha256=$BASE_SHA256
		built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
		recipe=packaging/images/postgres-18/build.sh
	MANIFEST

	if [ "$KEEP" -eq 0 ]; then
		rm -f "$BUILD_DISK" "$WORK_DIR/seed.iso"
		rm -rf "$WORK_DIR/seed"
	fi

	say ""
	say "image     $OUT"
	say "sha256    $sha"
	say "size      $((size / 1024 / 1024)) MiB on disk, $((virt / 1024 / 1024 / 1024)) GiB virtual"
	say "engine    postgres ${ENGINE_RELEASE:-unknown} (${ENGINE_PACKAGE:-unknown})"
	say "import with min_disk_gb=$mindisk"
	say "manifest  $OUT.manifest"
	say ""
	say "Verify it before publishing:  ./verify.sh --image $OUT"
}

need_tools
fetch_base
make_seed
run_build
finish
