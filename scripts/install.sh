#!/usr/bin/env bash
# install.sh -- install Kumo on this host from the latest GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/ahmedmohamedut/kumo/main/scripts/install.sh | sudo bash
#
# It picks the .deb or .rpm that matches this host, verifies it against the
# release's checksums.txt, and hands it to the system package manager. The
# package creates the service user, joins it to the libvirt group and applies
# the AppArmor or SELinux rules; this script only gets the right file onto the
# host and then tells you what to run.
#
# What it deliberately does not do: start the service. A node nobody has
# configured and nobody has run `kumod doctor` on should not begin serving by
# itself.
#
# Options:
#   --version X.Y.Z   install that release instead of the latest
#   --dry-run         print every step without changing anything
#   --help
set -euo pipefail
export LC_ALL=C

REPO=${KUMO_REPO:-ahmedmohamedut/kumo}
VERSION=${KUMO_VERSION:-latest}
DRY_RUN=0

say() { printf '%s\n' "$*"; }
err() { printf 'install.sh: %s\n' "$*" >&2; }
die() {
	err "$*"
	exit 1
}

# run executes a command, or prints it under --dry-run. Every change to the host
# goes through here, so --dry-run is honest by construction rather than by
# remembering to check a flag at each call site.
run() {
	if [ "$DRY_RUN" -eq 1 ]; then
		printf '  would run: %s\n' "$*"
		return 0
	fi
	"$@"
}

usage() {
	sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		VERSION=$2
		shift 2
		;;
	--version=*)
		VERSION=${1#*=}
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	--help | -h)
		usage
		exit 0
		;;
	*) die "unknown option $1 (try --help)" ;;
	esac
done

# --- what we are installing onto ---------------------------------------------

[ "$(uname -s)" = "Linux" ] || die "Kumo runs on Linux; this is $(uname -s)"

case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "no Kumo package for $(uname -m); only x86_64 and arm64 are built" ;;
esac

# The package manager decides the format, not the distribution name: a host can
# have apt without being Debian, and asking what is installed is more reliable
# than parsing /etc/os-release.
if command -v apt-get >/dev/null 2>&1; then
	FORMAT=deb
	INSTALL_CMD="apt-get install -y"
elif command -v dnf >/dev/null 2>&1; then
	FORMAT=rpm
	INSTALL_CMD="dnf install -y"
elif command -v zypper >/dev/null 2>&1; then
	FORMAT=rpm
	INSTALL_CMD="zypper --non-interactive install --allow-unsigned-rpm"
elif command -v yum >/dev/null 2>&1; then
	FORMAT=rpm
	INSTALL_CMD="yum install -y"
else
	die "no apt, dnf, zypper or yum here. Download the tar.gz from
       https://github.com/$REPO/releases and follow docs/install.md."
fi

if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
	die "run as root: curl -fsSL <url> | sudo bash"
fi

for tool in curl tar; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

if command -v sha256sum >/dev/null 2>&1; then
	SHA_CMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	SHA_CMD="shasum -a 256"
else
	die "sha256sum or shasum is required to verify the download"
fi

# --- which release ------------------------------------------------------------

if [ "$VERSION" = "latest" ]; then
	say "Looking up the latest Kumo release..."
	# The redirect of /releases/latest carries the tag, which avoids both the
	# JSON parsing and the lower rate limit of the API.
	LOCATION=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
		"https://github.com/$REPO/releases/latest") ||
		die "could not reach GitHub to find the latest release"
	TAG=${LOCATION##*/}
	[ -n "$TAG" ] && [ "$TAG" != "latest" ] ||
		die "no published release found for $REPO. Pass --version X.Y.Z."
else
	TAG="v${VERSION#v}"
fi

# The tag is v1.2.3; the artifacts are named with the bare version.
PLAIN=${TAG#v}
# KUMO_DOWNLOAD_BASE points the download somewhere else. It exists so this
# script's download-and-verify path can be tested against a local mirror
# without publishing a release; an installer whose checksum check has never
# been run is not a checksum check.
BASE="${KUMO_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download/$TAG}"

# Each format's own convention, which is what .goreleaser.yaml asks nfpm for:
# a deb carries the debian architecture and no os, an rpm carries its release
# number and the rpm architecture name.
case "$FORMAT" in
deb) ASSET="kumo_${PLAIN}_${ARCH}.deb" ;;
rpm)
	case "$ARCH" in
	amd64) ASSET="kumo-${PLAIN}-1.x86_64.rpm" ;;
	arm64) ASSET="kumo-${PLAIN}-1.aarch64.rpm" ;;
	esac
	;;
esac

say "Kumo $TAG, $FORMAT for $ARCH"

# --- download and verify ------------------------------------------------------

WORK=$(mktemp -d)
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

say "Downloading $ASSET..."
curl -fsSL --retry 3 -o "$WORK/$ASSET" "$BASE/$ASSET" ||
	die "could not download $BASE/$ASSET
       Check that $TAG has an asset for $FORMAT/$ARCH."

# An unverified package installed as root is the whole host. The checksums file
# is published with the release, so a corrupted or swapped asset is caught here.
say "Verifying the checksum..."
if curl -fsSL --retry 3 -o "$WORK/checksums.txt" "$BASE/checksums.txt"; then
	EXPECTED=$(grep " \{1,2\}$ASSET\$" "$WORK/checksums.txt" | awk '{print $1}' || true)
	if [ -z "$EXPECTED" ]; then
		die "checksums.txt does not list $ASSET; refusing to install it"
	fi
	ACTUAL=$($SHA_CMD "$WORK/$ASSET" | awk '{print $1}')
	if [ "$EXPECTED" != "$ACTUAL" ]; then
		die "checksum mismatch for $ASSET
       expected $EXPECTED
       got      $ACTUAL"
	fi
	say "  ok: $ACTUAL"
else
	die "could not download $BASE/checksums.txt; refusing to install unverified"
fi

# --- install ------------------------------------------------------------------

say "Installing with ${INSTALL_CMD%% *}..."
# shellcheck disable=SC2086 # INSTALL_CMD is a command with flags, split on purpose
run $INSTALL_CMD "$WORK/$ASSET"

if [ "$DRY_RUN" -eq 1 ]; then
	say ""
	say "Dry run: nothing was changed."
	exit 0
fi

say ""
say "Kumo $TAG is installed. Next:"
say ""
say "  sudo kumod doctor                      check this host can run guests"
say "  sudo systemctl enable --now kumod      start it"
say ""
say "Then open http://$(hostname -I 2>/dev/null | awk '{print $1}'):8080 and create the administrator."
say ""
say "This release serves plain HTTP. Keep it on a network you control, or put"
say "it behind a TLS proxy; /usr/share/doc/kumo/install.md explains the trade."
