#!/usr/bin/env bash
# preflight.sh -- refuse to build with an expired apt signing key.
#
# Both package repositories this recipe installs from are signed with keys
# pinned in files/, as the PGDG key is for Postgres, so the build trusts keys
# that were reviewed once:
#
#   kubernetes-apt-keyring.asc  pkgs.k8s.io (kubeadm, kubelet, kubectl, cri-tools)
#                               DE15 B144 86CD 377B 9E87  6E1A 2346 54DA 9A29 6436
#   cri-o-apt-keyring.asc       download.opensuse.org isv:cri-o (cri-o)
#                               85B6 7D5C 5010 0B1A C8CE  FE49 CBA9 C856 40A2 B579
#
# Both projects sign with keys that expire and are periodically extended. Once
# a pinned copy is past its date, apt in the guest refuses the repository with
# an EXPKEYSIG error minutes into a build; this says so up front, and warns a
# month ahead. Called by ../lib/build-image.sh before any download.
#
# The fix is to refresh the pinned copy from the repository's Release.key
#   https://pkgs.k8s.io/core:/stable:/v1.37/deb/Release.key
#   https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.37/deb/Release.key
# after checking that its fingerprint is still the one above.
set -euo pipefail
export LC_ALL=C

FILES=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/files

command -v gpg >/dev/null 2>&1 || { echo "preflight: gpg is required to check the pinned keys" >&2; exit 1; }

# check FILE FINGERPRINT
check() {
	local key=$FILES/$1 want=$2 info fpr expires now when
	info=$(gpg --show-keys --with-colons "$key" 2>/dev/null)
	fpr=$(awk -F: '/^fpr:/ {print $10; exit}' <<<"$info")
	[ "$fpr" = "$want" ] || { echo "preflight: $key has fingerprint ${fpr:-none}, want $want" >&2; return 1; }

	# Field 7 of the pub line is the expiry, in seconds since the epoch; empty
	# means the key does not expire.
	expires=$(awk -F: '/^pub:/ {print $7; exit}' <<<"$info")
	[ -n "$expires" ] || return 0
	now=$(date -u +%s)
	when=$(date -u -d "@$expires" +%Y-%m-%d)
	if [ "$expires" -le "$now" ]; then
		echo "preflight: the pinned key $1 expired on $when; refresh it (see the header of $0)" >&2
		return 1
	fi
	if [ $((expires - now)) -lt $((30 * 86400)) ]; then
		echo "preflight: warning: the pinned key $1 expires on $when; refresh it soon" >&2
		if [ -n "${GITHUB_ACTIONS:-}" ]; then
			echo "::warning::the pinned key packaging/images/kubernetes-1.37/files/$1 expires on $when; refresh it"
		fi
	fi
}

check kubernetes-apt-keyring.asc DE15B14486CD377B9E876E1A234654DA9A296436
check cri-o-apt-keyring.asc 85B67D5C50100B1AC8CEFE49CBA9C85640A2B579
