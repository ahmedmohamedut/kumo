#!/usr/bin/env bash
#
# Starts kumod for the Playwright suite: the real binary, the real API and the
# embedded console, with the in-memory hypervisor standing in for KVM so the
# run needs no libvirt and no root (AGENTS.md: "Never require real libvirt in
# unit tests", and a hosted runner has no /dev/kvm either).
#
# The data directory is wiped on every start rather than cleaned up on exit.
# The suite begins at the first-run screen, which only appears while the
# database holds no users, so a run that inherited the last one's database
# would fail on its first assertion. Wiping up front also leaves the database
# and the logs of a failed run in place to look at.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin/kumod"

if [[ ! -x "$BIN" ]]; then
	echo "e2e: $BIN is missing; run 'make build' first" >&2
	exit 1
fi

DATA_DIR="${KUMO_E2E_DATA_DIR:-${TMPDIR:-/tmp}/kumo-e2e}"
rm -rf "$DATA_DIR"
mkdir -p "$DATA_DIR"

# KUMO_IMAGE_ALLOW_PRIVATE_URLS lets the import reach the suite's loopback
# mirror (web/e2e/support/image-server.mjs). It is off by default as an SSRF
# guard, and this is the trusted-mirror case the variable exists for.
#
# KUMO_COOKIE_SECURE is false because the suite speaks plain HTTP; a Secure
# cookie would never be stored and every session would end at the first reload.
#
# KUMO_DEFAULT_VPC_CIDR is pinned away from the default 192.168.100.0/24, which
# is a common home-router range: the bootstrap refuses to create a default VPC
# that would shadow a host route, so on such a machine the node comes up with
# no default subnet and every launch is refused with a conflict. The suite must
# not depend on what the developer's LAN happens to use.
exec env \
	KUMO_LISTEN="127.0.0.1:${KUMO_E2E_PORT:-8099}" \
	KUMO_DATA_DIR="$DATA_DIR" \
	KUMO_HYPERVISOR=fake \
	KUMO_DEFAULT_VPC_CIDR="${KUMO_E2E_VPC_CIDR:-10.111.0.0/24}" \
	KUMO_COOKIE_SECURE=false \
	KUMO_IMAGE_ALLOW_PRIVATE_URLS=true \
	KUMO_LOG_LEVEL="${KUMO_E2E_LOG_LEVEL:-warn}" \
	"$BIN"
