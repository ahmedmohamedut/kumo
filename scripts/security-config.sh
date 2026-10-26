#!/usr/bin/env bash
# security_config.sh -- grant libvirt/qemu access to Kumo's storage pools under
# the host's mandatory access control (AppArmor or SELinux).
#
# Kumo's state lives under $KUMO_DATA_DIR (default /var/lib/kumo):
#   pools/images/     base images, read on every boot
#   pools/instances/  root disks and seed images, written by qemu-img / qemu
#   console/          serial console logs, written by virtlogd on qemu's behalf
#
# On a fresh install nothing under /var/lib carries a type a confined qemu or
# virtlogd is allowed to touch, so a first launch trips the MAC layer:
#   - AppArmor (Debian/Ubuntu): qemu-img is denied writing pools/instances;
#   - SELinux (RHEL/Fedora/SUSE): the same denial as an AVC, and virtlogd
#     (virtlogd_t) is denied writing console/.
#
# This script is the idempotent fix. On AppArmor hosts it adds Kumo's rules to
# the two local rule files under $AA_DIR/local/ (the mechanism the distro's
# libvirt profiles already include) and reloads the profiles. Those files are
# shared: libvirt creates them for the operator's own rules, so Kumo's lines
# live in a marked block and everything outside it is left alone. On SELinux
# hosts it records fcontext rules and relabels the trees:
#   pools/images, pools/instances -> virt_image_t  (qemu / svirt_t read+write)
#   console                       -> virt_log_t    (virtlogd_t creates logs)
# Existing rules carrying a different type are corrected, not skipped.
# Run it as root on every upgrade; it is part of the release package.
#
# Overrides:
#   KUMO_DATA_DIR  where Kumo keeps its state (default /var/lib/kumo)
#   KUMO_SERVICE_USER  the account kumod runs as (default kumo); it is given the
#                  pool and console directories, skipped when it does not exist
#   MAC            auto | apparmor | selinux | none   (default auto-detect)
#   AA_DIR         AppArmor profiles root (default /etc/apparmor.d)
set -euo pipefail
export LC_ALL=C

DATA_DIR="${KUMO_DATA_DIR:-/var/lib/kumo}"
SERVICE_USER="${KUMO_SERVICE_USER:-kumo}"
AA_DIR="${AA_DIR:-/etc/apparmor.d}"
AA_QEMU_LOCAL="$AA_DIR/local/abstractions/libvirt-qemu"
AA_HELPER_LOCAL="$AA_DIR/local/usr.lib.libvirt.virt-aa-helper"

log()  { printf 'security-config: %s\n' "$*"; }
warn() { printf 'security-config: warning: %s\n' "$*" >&2; }
die()  { printf 'security-config: error: %s\n' "$*" >&2; exit 1; }

[[ "$(id -u)" -eq 0 ]] || die "must run as root (sudo $0)"

# The markers of Kumo's block in a shared AppArmor local file. postremove.sh
# deletes the block by these exact lines, so change both together.
AA_BEGIN="# BEGIN kumo -- managed by security-config.sh, edits are overwritten"
AA_END="# END kumo"

# Put the rules on stdin into Kumo's block of a shared local file, keeping
# every other line. An earlier block is replaced, and the bare $DATA_DIR/ lines
# releases before the block existed wrote are folded into it. The file is only
# rewritten when its content changes, so repeated runs are quiet.
write_aa_block() {
	local path="$1" rules tmp
	rules="$(cat)"
	# No `trap ... RETURN` for the cleanup: in bash that trap is not local to
	# the function, so it fired again on every later function return, where
	# $tmp is out of scope, and `set -u` aborted the script with status 1.
	tmp="$(mktemp)"
	if [[ -f "$path" ]]; then
		awk -v begin="$AA_BEGIN" -v end="$AA_END" -v legacy="$DATA_DIR/" '
			$0 == begin { inside = 1; next }
			$0 == end   { inside = 0; next }
			inside      { next }
			index($0, legacy) == 1 { next }
			{ print }
		' "$path" > "$tmp"
	fi
	printf '%s\n%s\n%s\n' "$AA_BEGIN" "$rules" "$AA_END" >> "$tmp"
	if [[ -f "$path" ]] && cmp -s -- "$path" "$tmp"; then
		log "unchanged: $path"
	else
		install -m 644 -- "$tmp" "$path"
		log "wrote Kumo's rules into $path"
	fi
	rm -f -- "$tmp"
}

apparmor_enabled() {
	[[ -d "$AA_DIR" ]] || return 1
	if command -v aa-enabled >/dev/null 2>&1; then
		aa-enabled >/dev/null 2>&1
		return
	fi
	[[ -r /sys/module/apparmor/parameters/enabled ]] \
		&& [[ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" == "Y" ]]
}

selinux_enabled() {
	if command -v selinuxenabled >/dev/null 2>&1; then
		selinuxenabled >/dev/null 2>&1
		return
	fi
	[[ -e /sys/fs/selinux/enforce ]]
}

# A rule file only bites when the profile or abstraction that should carry it
# exists and actually includes the <local/...> file. Both hold on Debian and
# Ubuntu; anything else gets a warning, never a failure. AppArmor accepts the
# include with or without the leading '#', and with 'if exists' (Ubuntu 22.04
# and later write `include if exists <local/...>`), so match all of them.
require_local_include() {
	local hostpath="$1" include="$2" short
	short="${include#local/}"
	if [[ ! -f "$hostpath" ]]; then
		warn "AppArmor: $hostpath is missing, so the Kumo rule file $short has nothing to attach to yet"
		return 0
	fi
	grep -Eq "^[[:space:]]*#?include([[:space:]]+if[[:space:]]+exists)?[[:space:]]+<${include//./\\.}>" "$hostpath" \
		|| warn "AppArmor: $hostpath does not include <$include>; the Kumo rule file $short will not apply"
	return 0
}

reload_apparmor() {
	if command -v systemctl >/dev/null 2>&1 \
		&& systemctl is-active --quiet apparmor 2>/dev/null; then
		systemctl reload apparmor \
			&& log "AppArmor: profiles reloaded" \
			|| warn "systemctl reload apparmor failed; restart libvirtd or reboot to apply"
	elif command -v service >/dev/null 2>&1; then
		service apparmor reload \
			&& log "AppArmor: profiles reloaded" \
			|| warn "service apparmor reload failed; restart libvirtd or reboot to apply"
	else
		warn "no apparmor service reload found; the rules apply after a reboot"
	fi
}

configure_apparmor() {
	log "AppArmor: writing local libvirt rules for $DATA_DIR"
	mkdir -p "$AA_DIR/local/abstractions"

	# The libvirt-qemu abstraction confines the qemu processes that launch
	# guests (and the virt-aa-helper that writes the per-guest profile):
	# images are read-only, instance disks and the serial log are written.
	write_aa_block "$AA_QEMU_LOCAL" <<EOF
$DATA_DIR/pools/images/* rk,
$DATA_DIR/pools/instances/* rwk,
$DATA_DIR/console/* rw,
EOF

	write_aa_block "$AA_HELPER_LOCAL" <<EOF
$DATA_DIR/pools/images/* rk,
$DATA_DIR/pools/instances/* rk,
EOF

	require_local_include "$AA_DIR/abstractions/libvirt-qemu" \
		"local/abstractions/libvirt-qemu"
	require_local_include "$AA_DIR/usr.lib.libvirt.virt-aa-helper" \
		"local/usr.lib.libvirt.virt-aa-helper"

	reload_apparmor
}

selinux_mode() {
	# The mounted fs answers what the kernel is actually enforcing; getenforce
	# reports the configured mode even for kernels booted without SELinux.
	if [[ -r /sys/fs/selinux/enforce ]]; then
		[[ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" == "1" ]] \
			&& echo Enforcing || echo Permissive
	elif command -v getenforce >/dev/null 2>&1; then
		getenforce 2>/dev/null || echo Disabled
	else
		echo Disabled
	fi
}

# Ensure an fcontext rule exists for $1 with type $2. Adds a missing rule,
# modifies one carrying a different type, and leaves a correct one alone.
add_fcontext() {
	local target="$1" label="$2" line
	line="$(semanage fcontext -l -C 2>/dev/null | grep -F -- " $target " || true)"
	if [[ -z "$line" ]]; then
		semanage fcontext -a -t "$label" -- "$target" \
			&& log "SELinux: added fcontext rule $target -> $label" \
			|| die "could not add fcontext rule for $target"
	elif [[ "$line" != *":$label:"* ]]; then
		semanage fcontext -m -t "$label" -- "$target" \
			&& log "SELinux: updated fcontext rule $target -> $label" \
			|| die "could not modify fcontext rule for $target"
	else
		log "SELinux: fcontext rule already present for $target ($label)"
	fi
}

configure_selinux() {
	local mode
	mode="$(selinux_mode)"
	if [[ "$mode" == "Disabled" ]]; then
		log "SELinux: not active; nothing to label"
		return 0
	fi

	command -v semanage >/dev/null 2>&1 \
		|| die "semanage not found; install policycoreutils-python-utils (RHEL/Fedora) or python3-semanage (Debian/Ubuntu)"
	command -v restorecon >/dev/null 2>&1 \
		|| die "restorecon not found; install policycoreutils (RHEL/Fedora) or selinux-utils (Debian/Ubuntu)"

	# A container kernel often mounts selinuxfs without loading a policy: mode
	# reads Enforcing yet no store exists to manage. Ask semanage itself rather
	# than trusting the mode string.
	if ! semanage fcontext -l >/dev/null 2>&1; then
		warn "SELinux: the policy store is not accessible; no labels applied (a kernel without a loaded policy denies nothing)"
		return 0
	fi

	log "SELinux: $mode; labeling Kumo's trees (pools: virt_image_t, console: virt_log_t)"
	mkdir -p "$DATA_DIR/pools/images" "$DATA_DIR/pools/instances" "$DATA_DIR/console"

	# Disk images: read/written by qemu (svirt_t).
	add_fcontext "$DATA_DIR/pools/images(/.*)?"    virt_image_t
	add_fcontext "$DATA_DIR/pools/instances(/.*)?" virt_image_t
	# Serial console logs: created by virtlogd (virtlogd_t), which may write
	# virt_log_t but not virt_image_t.
	add_fcontext "$DATA_DIR/console(/.*)?"         virt_log_t

	if restorecon -Rv "$DATA_DIR"; then
		log "SELinux: relabeled $DATA_DIR"
	else
		warn "restorecon failed; relabel manually with: restorecon -Rv $DATA_DIR"
	fi
}

# Plain Unix permissions are checked independently of the MAC layer: libvirt
# probes disk images as the qemu user (uid 107 on Fedora/RHEL, libvirt-qemu on
# Debian/Ubuntu) before launching a guest, so that user must be able to
# traverse every directory down to the images. Add search (o+x) only; this
# does not make the directories listable or readable by others.
ensure_traversal() {
	local d
	mkdir -p "$DATA_DIR/pools/images" "$DATA_DIR/pools/instances" "$DATA_DIR/console"
	for d in "$DATA_DIR" "$DATA_DIR/pools" "$DATA_DIR/pools/images" "$DATA_DIR/pools/instances"; do
		if [[ "$(stat -c %a -- "$d")" != *[1357] ]]; then
			chmod o+x -- "$d"
			log "permissions: added search (o+x) on $d"
		fi
	done
	own_trees
}

# This script runs as root, so the directories it creates are root's, and an
# unprivileged kumod could neither import an image into pools/images nor write a
# seed disk or a console log. Hand the directories (not their contents, which
# libvirt and qemu own) to the service account. Skipped when there is none, e.g.
# a developer running kumod as themselves.
own_trees() {
	local group d
	id -u "$SERVICE_USER" >/dev/null 2>&1 || return 0
	group="$(id -gn "$SERVICE_USER")"
	for d in "$DATA_DIR/pools" "$DATA_DIR/pools/images" "$DATA_DIR/pools/instances" "$DATA_DIR/console"; do
		if [[ "$(stat -c %U:%G -- "$d")" != "$SERVICE_USER:$group" ]]; then
			chown "$SERVICE_USER:$group" -- "$d"
			log "permissions: gave $d to $SERVICE_USER:$group"
		fi
	done
}

ensure_traversal

case "${MAC:-auto}" in
apparmor)
	configure_apparmor
	;;
selinux)
	configure_selinux
	;;
none)
	log "MAC=none: nothing to do"
	;;
auto)
	aa=0
	se=0
	apparmor_enabled && aa=1
	selinux_enabled && se=1
	if [[ "$aa" -eq 1 && "$se" -eq 1 ]]; then
		warn "both AppArmor and SELinux look enabled; configuring both"
		configure_apparmor
		configure_selinux
	elif [[ "$aa" -eq 1 ]]; then
		configure_apparmor
	elif [[ "$se" -eq 1 ]]; then
		configure_selinux
	else
		log "no mandatory access control found; nothing to configure"
	fi
	;;
*)
	die "MAC must be auto, apparmor, selinux or none (got ${MAC:-})"
	;;
esac

log "done"
