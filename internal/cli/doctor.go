package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
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
	// note explains a failure, or says what is not checked yet.
	note string
	// pending marks a check a later milestone implements.
	pending bool
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
		checkSigningKey(cfg),
		checkHypervisor(cfg),
		{name: "libvirt socket", pending: true, note: "checked once the backend exists (M3.1)"},
		{name: "/dev/kvm", pending: true, note: "checked once the backend exists (M3.1)"},
		{name: "pools and network", pending: true, note: "checked once the backend exists (M3.1)"},
	}

	failed := 0
	for _, c := range checks {
		switch {
		case c.pending:
			fmt.Fprintf(out, "skip  %-18s %s\n", c.name, c.note)
		case c.ok:
			fmt.Fprintf(out, "ok    %-18s %s\n", c.name, c.note)
		default:
			failed++
			fmt.Fprintf(out, "FAIL  %-18s %s\n", c.name, c.note)
		}
	}

	fmt.Fprintln(out, "\nthe libvirt checks are not implemented yet; "+
		"see docs/design/04-libvirt-integration.md §8")

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

// checkDiskSpace warns when the data directory is nearly full.
func checkDiskSpace(dir string) check {
	free := freeBytes(dir)
	if free == 0 {
		return check{name: "disk space", ok: true, note: "not measurable on this platform"}
	}

	note := fmt.Sprintf("%.1f GiB free in %s", gib(free), dir)
	if free < minFreeBytes {
		return check{
			name: "disk space",
			ok:   false,
			note: note + fmt.Sprintf(", want at least %.0f GiB", gib(minFreeBytes)),
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

// checkSigningKey reports whether the key exists and is private, generating it
// when it is missing, so a first `kumod doctor` leaves the host ready to start.
func checkSigningKey(cfg *config.Config) check {
	if _, err := auth.LoadOrCreateSecret(cfg.JWTSecretFile); err != nil {
		return check{name: "jwt signing key", ok: false, note: err.Error()}
	}

	return check{name: "jwt signing key", ok: true, note: cfg.JWTSecretFile}
}

// checkHypervisor reports whether the selected backend is usable at all. The
// libvirt backend is refused by the server until M3.1, so doctor says so instead
// of pretending an instance could run.
func checkHypervisor(cfg *config.Config) check {
	if cfg.Hypervisor == config.HypervisorFake {
		return check{
			name: "hypervisor",
			ok:   true,
			note: "fake (in-memory, for development only)",
		}
	}

	return check{
		name: "hypervisor",
		ok:   false,
		note: "libvirt is configured but not implemented yet (M3.1); " +
			"set KUMO_HYPERVISOR=fake for development",
	}
}

func gib(bytes uint64) float64 { return float64(bytes) / (1 << 30) }
