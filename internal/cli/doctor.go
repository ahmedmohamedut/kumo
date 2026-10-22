package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"os/exec"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/store"
)

// minFreeBytes is the disk space a host should have in the data directory
// before importing images; 04-libvirt-integration.md §8 warns below 20 GiB.
const minFreeBytes = 20 << 30

// errChecksFailed is what doctor returns after reporting its failures, so the
// exit code says "something on this host needs attention".
var errChecksFailed = errors.New("checks failed")

// check is one line of `kumod doctor`.
type check struct {
	name string
	ok   bool
	// note explains a failure, or adds detail to a passing line.
	note string
	// warn marks a line that reports a risk without blocking the install.
	warn bool
}

// runDoctor implements `kumod doctor`. M1.2 only reports what does not need the
// hypervisor; the libvirt, KVM and firewall checks of 04-libvirt-integration.md
// §8 arrive with the real backend in M3.1.
func runDoctor(ctx context.Context, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("the configuration is not usable: %w", err)
	}

	checks := []check{
		{name: "configuration", ok: true, note: "the KUMO_* variables parse"},
		checkDataDir(cfg.DataDir),
		checkDiskSpace(cfg.DataDir),
		checkDatabase(ctx, cfg),
		checkDefaultVPC(ctx, cfg),
		checkSigningKey(cfg),
		checkHypervisor(cfg),
	}
	if cfg.Hypervisor == config.HypervisorLibvirt {
		checks = append(checks, checkLibvirt(ctx, cfg)...)
	}

	failed := 0
	for _, c := range checks {
		switch {
		case c.warn:
			fmt.Fprintf(out, "warn  %-18s %s\n", c.name, c.note)
		case c.ok:
			fmt.Fprintf(out, "ok    %-18s %s\n", c.name, c.note)
		default:
			failed++
			fmt.Fprintf(out, "FAIL  %-18s %s\n", c.name, c.note)
		}
	}

	if failed > 0 {
		return fmt.Errorf("%w: %d of %d", errChecksFailed, failed, len(checks))
	}

	return nil
}

// checkDataDir reports whether the data directory exists and is writable.
func checkDataDir(dir string) check {
	info, err := os.Stat(dir)
	if err != nil {
		return check{name: "data directory", ok: false, note: err.Error()}
	}
	if !info.IsDir() {
		return check{name: "data directory", ok: false, note: dir + " is not a directory"}
	}

	probe, err := os.CreateTemp(dir, ".doctor-*")
	if err != nil {
		return check{
			name: "data directory",
			ok:   false,
			note: fmt.Sprintf("%s is not writable: %v", dir, err),
		}
	}
	name := probe.Name()
	probe.Close()   //nolint:errcheck // a failed close does not change the verdict
	os.Remove(name) //nolint:errcheck // best effort

	return check{name: "data directory", ok: true, note: dir}
}

// checkDiskSpace warns when the data directory is nearly full. The design
// (04-libvirt-integration.md §8) calls this a warning, not a blocker: a host
// with 15 GiB left can still import a small image, and `doctor` should not fail
// a working install because of it.
func checkDiskSpace(dir string) check {
	free := freeBytes(dir)
	if free == 0 {
		return check{name: "disk space", ok: true, note: "not measurable on this platform"}
	}

	note := fmt.Sprintf("%.1f GiB free in %s", gib(free), dir)
	if free < minFreeBytes {
		return check{
			name: "disk space",
			ok:   true,
			warn: true,
			note: note + fmt.Sprintf(", below the recommended %.0f GiB", gib(minFreeBytes)),
		}
	}

	return check{name: "disk space", ok: true, note: note}
}

// checkDatabase opens the database, so a broken migration or an unwritable file
// is reported before the server tries to start.
func checkDatabase(ctx context.Context, cfg *config.Config) check {
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return check{name: "database", ok: false, note: err.Error()}
	}
	defer db.Close() //nolint:errcheck // a failed close does not change the verdict

	users, err := db.CountUsers(ctx)
	if err != nil {
		return check{name: "database", ok: false, note: "cannot read users: " + err.Error()}
	}

	state := "first-run setup is open: POST /api/v1/auth/setup or `kumod admin create`"
	if users > 0 {
		state = fmt.Sprintf("%d user(s) exist", users)
	}

	return check{name: "database", ok: true, note: fmt.Sprintf("%s (%s)", cfg.DBPath, state)}
}

// checkDefaultVPC warns when the first start would not create a default VPC
// because its range is already routed on this host. It is a warning, not a
// failure: the service starts anyway, and the operator creates a VPC in the
// console (or sets KUMO_DEFAULT_VPC_CIDR).
func checkDefaultVPC(ctx context.Context, cfg *config.Config) check {
	const name = "default VPC"

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "not checked: " + err.Error()}
	}
	defer db.Close() //nolint:errcheck // a failed close does not change the verdict

	if existing, err := db.GetDefaultVPC(ctx); err == nil {
		return check{name: name, ok: true, note: fmt.Sprintf("%s (%s)", existing.Name, existing.Cidr)}
	}

	prefix, err := netip.ParsePrefix(cfg.DefaultVPCCIDR)
	if err != nil {
		return check{name: name, ok: false, note: err.Error()}
	}
	collision, err := network.HostCollision(prefix)
	switch {
	case err != nil:
		return check{name: name, ok: true, warn: true, note: "could not scan the host routes: " + err.Error()}
	case collision != "":
		return check{
			name: name,
			ok:   true,
			warn: true,
			note: fmt.Sprintf("%s overlaps %s, so none will be created on start; "+
				"create a VPC in the console, or set KUMO_DEFAULT_VPC_CIDR", prefix, collision),
		}
	}
	return check{name: name, ok: true, note: fmt.Sprintf("%s will be created on first start", prefix)}
}

// checkSigningKey reports whether the key exists and is private, generating it
// when it is missing, so a first `kumod doctor` leaves the host ready to start.
func checkSigningKey(cfg *config.Config) check {
	if _, err := auth.LoadOrCreateSecret(cfg.JWTSecretFile); err != nil {
		return check{name: "jwt signing key", ok: false, note: err.Error()}
	}

	return check{name: "jwt signing key", ok: true, note: cfg.JWTSecretFile}
}

// checkHypervisor reports which backend is selected. The per-host libvirt
// checks live in checkLibvirt; this line only names the backend so a
// misconfigured KUMO_HYPERVISOR is visible at a glance.
func checkHypervisor(cfg *config.Config) check {
	switch cfg.Hypervisor {
	case config.HypervisorFake:
		return check{
			name: "hypervisor",
			ok:   true,
			note: "fake (in-memory, for development only)",
		}
	case config.HypervisorLibvirt:
		return check{name: "hypervisor", ok: true, note: cfg.LibvirtURI}
	default:
		return check{
			name: "hypervisor",
			ok:   false,
			note: fmt.Sprintf("%q names no known backend", cfg.Hypervisor),
		}
	}
}

func gib(bytes uint64) float64 { return float64(bytes) / (1 << 30) }

// checkLibvirt runs the host checks of 04-libvirt-integration.md §8 that need a
// real daemon: /dev/kvm, the socket, QEMU, the pools and the network. Each
// failure carries the remediation, because a half-configured libvirt host is
// the single most common reason kumod will not start.
func checkLibvirt(ctx context.Context, cfg *config.Config) []check {
	cks := make([]check, 0, 6)

	cks = append(cks, checkKVM())

	client, err := libvirt.New(logging.New(slog.LevelWarn, io.Discard), cfg.LibvirtURI)
	if err != nil {
		return append(cks, check{name: "libvirt socket", ok: false, note: err.Error()})
	}
	defer client.Close() //nolint:errcheck // the process exits right after

	if err := client.Connect(ctx); err != nil {
		return append(cks, check{
			name: "libvirt socket",
			ok:   false,
			note: fmt.Sprintf("%s: %v. Is libvirtd running, "+
				"and is the kumo user in the libvirt group?", cfg.LibvirtURI, err),
		})
	}

	if err := client.Ping(ctx); err != nil {
		return append(cks, check{name: "libvirt socket", ok: false, note: err.Error()})
	}

	cks = append(cks,
		check{name: "libvirt socket", ok: true, note: cfg.LibvirtURI},
		checkQEMU(),
	)

	// EnsureInfra is idempotent, so doctor uses it as its own probe: it both
	// reports a missing pool and leaves the host ready for the server to start.
	infra := hypervisor.InfraConfig{
		ImagesPool:    cfg.PoolImages,
		InstancesPool: cfg.PoolInstances,
		Network:       cfg.Network,
		DataDir:       cfg.DataDir,
	}

	if err := client.EnsureInfra(ctx, infra); err != nil {
		cks = append(cks, check{
			name: "pools and network",
			ok:   false,
			note: fmt.Sprintf("%v. Check that %s is writable by the qemu user "+
				"and that the kumo user may define networks.",
				err, cfg.DataDir),
		})
	} else {
		cks = append(cks, check{
			name: "pools and network",
			ok:   true,
			note: fmt.Sprintf("%s, %s, %s", infra.ImagesPool, infra.InstancesPool, infra.Network),
		})
	}

	return append(cks, checkIPForward())
}

// checkKVM reports whether hardware acceleration is usable. Without it the
// control plane still runs - images import, the console works - but no guest
// will ever boot, so it is reported as a blocking problem with the fix.
func checkKVM() check {
	if libvirt.HasKVM() {
		return check{name: "/dev/kvm", ok: true, note: "present and accessible"}
	}

	return check{
		name: "/dev/kvm",
		ok:   false,
		note: "not present; instances cannot run. Load the kvm-intel or kvm-amd " +
			"module and give the service user access: usermod -aG kvm,libvirt kumo",
	}
}

// checkQEMU reports whether a QEMU binary libvirt can use exists. qemu-img is
// only reported, never required: Kumo parses qcow2 headers itself.
func checkQEMU() check {
	const note = "install qemu-system-x86 and qemu-utils"

	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		if _, err := exec.LookPath("qemu-kvm"); err != nil {
			return check{name: "qemu", ok: false, note: "no qemu-system-x86_64 in PATH; " + note}
		}
	}
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return check{name: "qemu", ok: true, warn: true, note: "qemu-img not in PATH (optional); " + note}
	}

	return check{name: "qemu", ok: true, note: "qemu-system-x86_64 and qemu-img in PATH"}
}

// checkIPForward reports the sysctl libvirt NAT needs. It is only a warning:
// the network may already be routed by other means, and libvirt sets it itself
// on most hosts.
func checkIPForward() check {
	const path = "/proc/sys/net/ipv4/ip_forward"

	raw, err := os.ReadFile(path)
	if err != nil {
		return check{name: "ip_forward", ok: true, warn: true, note: path + " is not readable"}
	}

	if value := strings.TrimSpace(string(raw)); value != "1" {
		return check{
			name: "ip_forward",
			ok:   true,
			warn: true,
			note: value + "; guests will not reach the network. " +
				"Enable it: sysctl -w net.ipv4.ip_forward=1",
		}
	}

	return check{name: "ip_forward", ok: true, note: "enabled"}
}
