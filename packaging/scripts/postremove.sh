#!/bin/sh
# Runs after the package's files are gone, on both .deb and .rpm.
#
# Removing Kumo removes everything it made: the guests it defined, its libvirt
# pools and network, /var/lib/kumo (the database, the signing key, the images
# and the root disks), the SELinux/AppArmor rules and the service account. That
# is destructive on purpose; it is what `rpm -e` / `apt remove` is documented to
# do (docs/install.md). An upgrade is a remove followed by an install, so none
# of it runs unless this is a real removal.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

# rpm passes 0 on a real removal and 1 or more on an upgrade; dpkg passes a word.
case "${1:-}" in
0 | remove | purge) ;;
*) exit 0 ;;
esac

KUMO_HOME=/var/lib/kumo

# --- libvirt: guests, pools, network -----------------------------------------
# Guests are found by name: a Kumo domain is named after its instance id
# (i- plus 17 hex). Anything else belongs to someone else and is left alone.
# Best effort throughout: libvirtd may be stopped, and the data directory has to
# go regardless.
if command -v virsh >/dev/null 2>&1; then
	VIRSH="virsh -c qemu:///system"
	for dom in $($VIRSH list --all --name 2>/dev/null | grep -E '^i-[0-9a-f]{17}$' || true); do
		$VIRSH destroy "$dom" >/dev/null 2>&1 || true
		$VIRSH undefine "$dom" --nvram --managed-save --snapshots-metadata >/dev/null 2>&1 ||
			$VIRSH undefine "$dom" >/dev/null 2>&1 || true
		echo "kumo: removed instance $dom"
	done
	for pool in kumo-instances kumo-images; do
		$VIRSH pool-destroy "$pool" >/dev/null 2>&1 || true
		$VIRSH pool-undefine "$pool" >/dev/null 2>&1 || true
	done
	# Every VPC is its own libvirt network, named after the VPC id (vpc- plus 17
	# hex), and destroying the network takes its bridge (kmb plus 8 hex) with it.
	# kumo-default is the network of a default VPC adopted from an earlier setup.
	for net in $($VIRSH net-list --all --name 2>/dev/null | grep -E '^(vpc-[0-9a-f]{17}|kumo-default)$' || true); do
		$VIRSH net-destroy "$net" >/dev/null 2>&1 || true
		$VIRSH net-undefine "$net" >/dev/null 2>&1 || true
		echo "kumo: removed network $net"
	done
fi

# A bridge libvirt did not take down (libvirtd stopped, or the network was
# already undefined) is deleted by name. Only Kumo's kmb<8 hex> bridges match.
if command -v ip >/dev/null 2>&1; then
	for br in $(ip -o link show type bridge 2>/dev/null | awk -F': ' '{print $2}' | grep -E '^kmb[0-9a-f]{8}$' || true); do
		ip link set "$br" down >/dev/null 2>&1 || true
		ip link delete "$br" type bridge >/dev/null 2>&1 || true
		echo "kumo: removed bridge $br"
	done
fi

# --- mandatory access control ------------------------------------------------
# security-config.sh is already gone, so undo what it did inline.
if command -v semanage >/dev/null 2>&1; then
	for target in "$KUMO_HOME/pools/images(/.*)?" "$KUMO_HOME/pools/instances(/.*)?" "$KUMO_HOME/console(/.*)?"; do
		semanage fcontext -d -- "$target" >/dev/null 2>&1 || true
	done
fi
for f in /etc/apparmor.d/local/abstractions/libvirt-qemu /etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper; do
	[ -f "$f" ] && sed -i "\\#^$KUMO_HOME/#d" "$f" || true
done

# --- state, configuration, account -------------------------------------------
rm -rf "$KUMO_HOME" /etc/kumo

if getent passwd kumo >/dev/null 2>&1; then
	userdel kumo >/dev/null 2>&1 || true
fi
if getent group kumo >/dev/null 2>&1; then
	groupdel kumo >/dev/null 2>&1 || true
fi

echo "kumo: removed, including /var/lib/kumo, its libvirt guests, pools and network, and the kumo user."
