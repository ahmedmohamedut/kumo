#!/usr/bin/env bash
# security-config.sh -- grant libvirt/qemu access to Kumo's storage pools under
# the host's mandatory access control (AppArmor or SELinux).
#
# Kumo's state lives under $KUMO_DATA_DIR (default /var/lib/kumo):
#   pools/images/     base images, read on every boot
#   pools/instances/  root disks and seed images, written by qemu-img
#   console/          serial console logs, written by qemu
#
# On a fresh install nothing under /var/lib carries a type a confined qemu is
# allowed to touch, so a first launch trips the MAC layer:
#   - AppArmor (Debian/Ubuntu): qemu-img is denied writing pools/instances;
#   - SELinux (RHEL/Fedora/SUSE): same denial, as an AVC.
#
# This script is the idempotent fix. On AppArmor hosts it writes the two local
# rule files under $AA_DIR/local/ (the mechanism the distro's libvirt
# profiles already include) and reloads the profiles. On SELinux hosts it
# records fcontext rules and relabels the trees virt_image_t, the type a qemu
# domain (svirt_t) may read and write. Run it as root on every upgrade; it is
# part of the release package.
#
# Overrides:
#   KUMO_DATA_DIR  where Kumo keeps its state (default /var/lib/kumo)
#   MAC            auto | apparmor | selinux | none   (default auto-detect)
#   AA_DIR         AppArmor profiles root (default /etc/apparmor.d)
set -euo pipefail
export LC_ALL=C

DATA_DIR="${KUMO_DATA_DIR:-/var/lib/kumo}"
AA_DIR="${AA_DIR:-/etc/apparmor.d}"
AA_QEMU_LOCAL="$AA_DIR/local/abstractions/libvirt-qemu"
AA_HELPER_LOCAL="$AA_DIR/local/usr.lib.libvirt.virt-aa-helper"

log()  { printf 'security-config: %s\n' "$*"; }
warn() { printf 'security-config: warning: %s\n' "$*" >&2; }
die()  { printf 'security-config: error: %s\n' "$*" >&2; exit 1; }

[[ "$(id -u)" -eq 0 ]] || die "must run as root (sudo $0)"

# Write a heredoc to a file only when its content changed, so repeated runs are
# quiet and never clobber an identical rule set.
write_if_changed() {
	local path="$1" tmp
	tmp="$(mktemp)"
	cat > "$tmp"
	if [[ -f "$path" ]] && cmp -s -- "$path" "$tmp"; then
		rm -f -- "$tmp"
		log "unchanged: $path"
	else
		mv -- "$tmp" "$path"
		chmod 644 -- "$path"
		log "wrote $path"
	fi
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
# Ubuntu; anything else gets a warning, never a failure.
require_local_include() {
	local hostpath="$1" include="$2" short
	short="${include#local/}"
	if [[ ! -f "$hostpath" ]]; then
		warn "AppArmor: $hostpath is missing, so the Kumo rule file $short has nothing to attach to yet"
		return 0
	fi
	grep -Fq "#include <$include>" "$hostpath" \
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
	write_if_changed "$AA_QEMU_LOCAL" <<EOF
$DATA_DIR/pools/images/* rk,
$DATA_DIR/pools/instances/* rwk,
$DATA_DIR/console/* rw,
EOF

	write_if_changed "$AA_HELPER_LOCAL" <<EOF
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

add_fcontext() {
	local target="$1" label="$2"
	if semanage fcontext -l -C 2>/dev/null | grep -Fq -- " $target "; then
		log "SELinux: fcontext rule already present for $target"
	else
		semanage fcontext -a -t "$label" -- "$target" \
			&& log "SELinux: added fcontext rule $target -> $label" \
			|| die "could not add fcontext rule for $target"
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

	log "SELinux: $mode; labeling Kumo's trees as virt_image_t"
	mkdir -p "$DATA_DIR/pools/images" "$DATA_DIR/pools/instances" "$DATA_DIR/console"

	add_fcontext "$DATA_DIR/pools/images(/.*)?" virt_image_t
	add_fcontext "$DATA_DIR/pools/instances(/.*)?" virt_image_t
	add_fcontext "$DATA_DIR/console(/.*)?" virt_image_t

	if restorecon -Rv "$DATA_DIR"; then
		log "SELinux: relabeled $DATA_DIR"
	else
		warn "restorecon failed; relabel manually with: restorecon -Rv $DATA_DIR"
	fi
}

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