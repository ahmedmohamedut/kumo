#!/usr/bin/env bash
# guest-finish.sh -- the teardown every Kumo engine image ends its build with.
#
# Runs INSIDE the build VM, as the last step of a recipe's provision script,
# from the read-only KUMOFILES ISO build-image.sh attaches:
#
#   bash /run/kumo-files/lib/guest-finish.sh
#
# By then the recipe has installed its engine and printed KUMO-IMAGE-ENGINE and
# KUMO-IMAGE-PKG to the console. This script makes the disk a golden image:
# one kernel and no headers, no package caches, no machine identity, no logs,
# freed blocks handed back to the qcow2. It prints KUMO-IMAGE-READY after
# everything that can fail, then removes cloud-init's state and powers off.
# build-image.sh tells a finished image from an abandoned one by that marker.
#
# One copy for every recipe, because the size of the Postgres image was decided
# by exactly these steps (packaging/images/README.md) and two copies would
# drift.
set -euo pipefail
export LC_ALL=C
export DEBIAN_FRONTEND=noninteractive

trap 'printf "\nKUMO-IMAGE-FAILED\n" >/dev/console; sync; systemctl --no-block poweroff' ERR

# One kernel, no headers. package_upgrade installs a newer kernel beside the
# one this VM booted, and autoremove never takes the running kernel, so
# without this the image ships two kernels, two module trees, two header trees
# and two initrds: about 256 MiB, enough to push the Postgres image past
# GitHub's 2 GiB release asset limit. The headers go entirely, because an
# engine image never builds a module. linux-image-virtual is marked manual
# first, so that dropping the linux-virtual metapackage (which is what pulls
# the headers) cannot let autoremove take the kernel with it.
running=$(uname -r)
newest=$(find /boot -maxdepth 1 -name 'vmlinuz-*-*' -printf '%f\n' | sed 's/^vmlinuz-//' | sort -V | tail -1)
apt-mark manual linux-image-virtual "linux-image-$newest" "linux-modules-$newest" >/dev/null
stale=$(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' 'linux-headers-*' 'linux-tools-[0-9]*' 2>/dev/null | awk '$1 == "ii" {print $2}')
if [ "$running" != "$newest" ]; then
	old=${running%-generic}
	# Removing the running kernel can ask "abort kernel removal?", whose
	# default under a noninteractive frontend is yes. Answered no under both
	# names the question has gone by; an unknown name is stored and ignored.
	printf '%s\n' \
		'linux-base linux-base/removing-running-kernel boolean false' \
		"linux-image-$running linux-image-$running/prerm/removing-running-kernel-$running boolean false" |
		debconf-set-selections
	stale="$stale $(dpkg-query -W -f='${db:Status-Abbrev} ${Package}\n' "linux-*-$old" "linux-*-$old-generic" 2>/dev/null | awk '$1 == "ii" {print $2}')"
fi
# shellcheck disable=SC2086 # one package name per word, on purpose
[ -z "${stale//[[:space:]]/}" ] || apt-get -y purge linux-virtual $stale

apt-get -y autoremove --purge

kernels=$(find /boot -maxdepth 1 -name 'vmlinuz-*-*' -printf '%f\n')
if [ "$kernels" != "vmlinuz-$newest" ] || [ ! -e "/boot/initrd.img-$newest" ] ||
	dpkg-query -W -f='${db:Status-Abbrev}\n' 'linux-headers-*' 2>/dev/null | grep -q '^ii'; then
	printf 'kernel cleanup left: %s\n' "$kernels" >/dev/console
	false
fi
printf 'KUMO-IMAGE-KERNEL=%s\n' "$newest" >/dev/console

apt-get -y clean
rm -rf /var/lib/apt/lists/*

# Identity. A clone sharing a machine-id or an SSH host key is a bug that
# presents as magic, so each is removed rather than regenerated here: systemd
# and cloud-init recreate theirs on first boot. machine-id is emptied and not
# deleted, because systemd wants the file to exist.
: >/etc/machine-id
rm -f /var/lib/dbus/machine-id
rm -f /etc/ssh/ssh_host_*

rm -rf /var/log/journal/*
find /var/log -type f -exec truncate -s 0 {} +
rm -f /root/.bash_history /home/ubuntu/.bash_history

# Hand the freed blocks back to the qcow2 so the convert in build-image.sh can
# drop them. The build disk is attached with discard=unmap for this.
fstrim -av || true
sync

# Where the bytes are, in the serial log, for whoever next finds the image
# larger than it was.
{
	printf '\nKUMO-IMAGE-SIZE-REPORT\n'
	du -xsm /usr /var /boot /opt /snap /var/lib/snapd /var/lib/containerd /var/lib/containers 2>/dev/null | sort -rn
	dpkg-query -W -f='${Installed-Size}\t${Package}\n' | sort -rn | head -15
	df -m / /boot
} >/dev/console 2>&1 || true

printf 'KUMO-IMAGE-READY\n' >/dev/console

# Last, because it deletes the state directory cloud-init is running out of:
# without it the next boot would recognise its own instance-id and skip the
# seed every guest is configured by.
cloud-init clean --logs --seed
sync
systemctl --no-block poweroff
