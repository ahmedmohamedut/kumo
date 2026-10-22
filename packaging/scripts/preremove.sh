#!/bin/sh
# Runs before the package's files are taken away, on both .deb and .rpm.
#
# The service is stopped because its binary is about to disappear; it is
# disabled so an upgrade that renames the unit does not leave a dangling
# symlink. Nothing here touches /var/lib/kumo — a removal must not take an
# operator's instances with it.
set -e

# An upgrade runs this too: rpm passes the number of copies that will remain
# (0 on a real removal, 1 or more on an upgrade) and dpkg passes a word. Only a
# removal stops and disables the service; an upgrade must leave it running.
case "${1:-}" in
0 | remove | purge | deconfigure) ;;
*) exit 0 ;;
esac

if command -v systemctl >/dev/null 2>&1; then
	systemctl stop kumod >/dev/null 2>&1 || true
	systemctl disable kumod >/dev/null 2>&1 || true
fi
