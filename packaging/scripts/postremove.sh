#!/bin/sh
# Runs after the package's files are gone, on both .deb and .rpm.
#
# The database, the signing key, the storage pools and therefore every instance
# live in /var/lib/kumo, and the kumo user owns them. Both are deliberately
# left behind: a package removal is not a request to destroy the guests running
# on this node, and an upgrade is a remove followed by an install. What to do
# with them is said out loud instead of guessed at.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

if [ -d /var/lib/kumo ]; then
	cat <<'MSG'

Kumo is removed. Its state was kept:

  /var/lib/kumo    the database, the signing key, the images and the root disks

The instances it defined are still defined in libvirt and are still running.
To remove them, destroy and undefine them with virsh before deleting the
directory and the "kumo" user.
MSG
fi
