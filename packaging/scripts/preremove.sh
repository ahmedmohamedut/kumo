#!/bin/sh
# Runs before the package's files are taken away, on both .deb and .rpm.
#
# The service is stopped because its binary is about to disappear; it is
# disabled so an upgrade that renames the unit does not leave a dangling
# symlink. Nothing here touches /var/lib/kumo — a removal must not take an
# operator's instances with it.
set -e

if command -v systemctl >/dev/null 2>&1; then
	systemctl stop kumod >/dev/null 2>&1 || true
	systemctl disable kumod >/dev/null 2>&1 || true
fi
