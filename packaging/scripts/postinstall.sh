#!/bin/sh
# Runs after the package is unpacked, on both .deb and .rpm.
#
# Three things have to be true before kumod can start, and none of them can be
# shipped inside a package: the service user has to exist, it has to be in the
# group that owns libvirt's socket, and the storage pools have to carry a label
# a confined qemu is allowed to write (scripts/security-config.sh explains why).
#
# POSIX sh, not bash: this runs on whatever the host has.
set -e

KUMO_USER=kumo
KUMO_GROUP=kumo
KUMO_HOME=/var/lib/kumo

# --- the service account -----------------------------------------------------
# A system account with no login shell and no password. Created rather than
# required, and left alone if an operator made it first.
if ! getent group "$KUMO_GROUP" >/dev/null 2>&1; then
	groupadd --system "$KUMO_GROUP"
fi

if ! getent passwd "$KUMO_USER" >/dev/null 2>&1; then
	useradd \
		--system \
		--gid "$KUMO_GROUP" \
		--home-dir "$KUMO_HOME" \
		--no-create-home \
		--shell /usr/sbin/nologin \
		--comment "Kumo control plane" \
		"$KUMO_USER"
fi

# --- reaching libvirt --------------------------------------------------------
# The socket is group-owned, and distributions disagree on the name: Debian and
# Fedora use "libvirt", some others "libvirtd". Join whichever exists. Without
# this, `kumod doctor` reports the socket as unreachable and nothing can boot.
for group in libvirt libvirtd kvm; do
	if getent group "$group" >/dev/null 2>&1; then
		usermod --append --groups "$group" "$KUMO_USER" || true
	fi
done

# --- the state directory -----------------------------------------------------
# systemd's StateDirectory creates this on start, but doctor and `kumod admin
# create` are run by an operator before the first start, so it has to exist now.
mkdir -p "$KUMO_HOME"
chown "$KUMO_USER":"$KUMO_GROUP" "$KUMO_HOME"
chmod 0750 "$KUMO_HOME"

# --- mandatory access control ------------------------------------------------
# AppArmor and SELinux both deny a confined qemu access to a fresh directory
# under /var/lib. The script is idempotent and detects which layer is in use;
# it is not fatal if it cannot finish, because an operator can run it by hand
# and the failure is worth seeing rather than hiding.
if [ -x /usr/libexec/kumo/security-config.sh ]; then
	if ! KUMO_DATA_DIR="$KUMO_HOME" /usr/libexec/kumo/security-config.sh; then
		echo "kumo: could not apply the AppArmor/SELinux rules for $KUMO_HOME." >&2
		echo "kumo: run /usr/libexec/kumo/security-config.sh by hand before starting." >&2
	fi
fi

# --- systemd ------------------------------------------------------------------
# Reloaded, never started. Starting a service the operator has not configured
# is the kind of surprise that makes people distrust a package; the next step is
# printed instead.
if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
fi

cat <<'EOF'

Kumo is installed. Two steps left:

  1. sudo kumod doctor          check this host can run guests
  2. sudo systemctl enable --now kumod

Then open http://<this-host>:8080 and create the administrator.

Configuration lives in /etc/kumo/kumod.env; every setting is documented there.
EOF
