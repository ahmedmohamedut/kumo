package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
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
	checks = append(checks, checkObjectStore(ctx, cfg)...)
	checks = append(checks, checkDatabases(ctx, cfg), checkKubernetes(ctx, cfg), checkSpark(ctx, cfg))

	// Said before the results rather than after them: an operator who ran
	// `kumod doctor` as themselves reads FAIL lines first otherwise, and
	// those look like a broken install when they are only this account's.
	if hint := accountHint(cfg.DataDir, os.Geteuid()); hint != "" {
		fmt.Fprintf(out, "note  %-18s %s\n", "account", hint)
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

// accountHint explains failures, and damage, that come from running doctor as
// the wrong account. The data directory belongs to the service user (kumo, on a
// packaged node) and holds files only that user may read, such as the 0600
// signing key, so checks that pass for the service fail for anybody else. Root
// reads everything, but the database and signing key doctor creates would then
// belong to root and the service could not write them (install.md). It returns
// "" when the account owns the directory or the owner cannot be read.
func accountHint(dataDir string, euid int) string {
	owner, ok := ownerUID(dataDir)
	if !ok || owner == euid {
		return ""
	}

	ownerName := strconv.Itoa(owner)
	if u, err := user.LookupId(ownerName); err == nil {
		ownerName = u.Username
	}
	run := "run: sudo -u " + ownerName + " kumod doctor"

	if euid == 0 {
		return fmt.Sprintf("running as root, but %s belongs to %s: files doctor "+
			"creates there would belong to root and the service could not write "+
			"them; %s", dataDir, ownerName, run)
	}

	self := strconv.Itoa(euid)
	if u, err := user.LookupId(self); err == nil {
		self = u.Username
	}
	return fmt.Sprintf("running as %s, but %s belongs to %s, so checks that read "+
		"its files fail for this account only; %s", self, dataDir, ownerName, run)
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

// checkObjectStore reports the optional object storage module. Off is one
// passing line; on, an unreachable server or an unsafe credentials file
// fails, because the operator asked for the module and it would not work.
func checkObjectStore(ctx context.Context, cfg *config.Config) []check {
	const name = "object store"
	switch cfg.ObjectStore {
	case config.ObjectStoreNone:
		return []check{{name: name, ok: true, note: "off (set KUMO_OBJECTSTORE=rustfs to enable)"}}
	case config.ObjectStoreFake:
		return []check{{name: name, ok: true, note: "fake (in-memory, for development only)"}}
	}

	store, err := newObjectStore(cfg)
	if err != nil {
		return []check{{name: name, ok: false, note: err.Error()}}
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := store.Ping(pingCtx); err != nil {
		return []check{{name: name, ok: false, note: fmt.Sprintf("%s: %v", cfg.S3Endpoint, err)}}
	}

	checks := []check{{name: name, ok: true, note: cfg.S3Endpoint + " answers with the configured keys"}}
	if len(cfg.S3CORSOrigins) == 0 {
		checks = append(checks, check{
			name: "s3 cors origins", ok: true, warn: true,
			note: "KUMO_S3_CORS_ORIGINS is empty: browsers will refuse console uploads",
		})
	}
	return checks
}

// checkDatabases reports the managed Postgres module. Unlike object storage it
// depends on no outside server, so there is nothing here that can fail: with
// the module off it is one passing line, and with it on but no engine image
// imported it warns, because the operator left it on and no DB instance can be
// created until a golden image exists (docs/design/07-databases.md section 2).
func checkDatabases(ctx context.Context, cfg *config.Config) check {
	const name = "databases"
	if !cfg.Databases {
		return check{name: name, ok: true, note: "off (set KUMO_DATABASES=on to enable)"}
	}

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "not checked: " + err.Error()}
	}
	defer db.Close() //nolint:errcheck // a failed close does not change the verdict

	all, err := db.ListAvailableEngineImages(ctx)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "cannot read images: " + err.Error()}
	}
	// Only the engines a DB instance can boot: a Kubernetes node image is an
	// engine image too, and would otherwise satisfy this check on a node that
	// cannot create a single database.
	rows := all[:0]
	for _, row := range all {
		if row.Engine.Valid && image.Engine(row.Engine.String).IsDatabase() {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return check{
			name: name, ok: true, warn: true,
			note: "on, but no engine image is available: import one before creating a " +
				"DB instance (see packaging/images/postgres-18/README.md)",
		}
	}

	engines := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if !row.Engine.Valid || !row.EngineVersion.Valid {
			continue
		}
		label := row.Engine.String + " " + row.EngineVersion.String
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		engines = append(engines, label)
	}
	return check{name: name, ok: true, note: "on (" + strings.Join(engines, ", ") + ")"}
}

// checkKubernetes reports the Kubernetes module the way checkDatabases reports
// the databases one: a warning, not a failure, when it is on and no Kubernetes
// image is available (docs/design/08-kubernetes.md section 2).
func checkKubernetes(ctx context.Context, cfg *config.Config) check {
	const name = "kubernetes"
	if !cfg.Kubernetes {
		return check{name: name, ok: true, note: "off (set KUMO_KUBERNETES=on to enable)"}
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "not checked: " + err.Error()}
	}
	defer db.Close() //nolint:errcheck // a failed close does not change the verdict

	rows, err := db.ListAvailableEngineImages(ctx)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "cannot read images: " + err.Error()}
	}
	var versions []string
	for _, row := range rows {
		if row.Engine.Valid && image.Engine(row.Engine.String) == image.EngineKubernetes && row.EngineVersion.Valid {
			versions = append(versions, row.EngineVersion.String)
		}
	}
	if len(versions) == 0 {
		return check{
			name: name, ok: true, warn: true,
			note: "on, but no Kubernetes image is available: import one before creating a " +
				"cluster (see packaging/images/kubernetes-1.37/README.md)",
		}
	}
	return check{name: name, ok: true, note: "on (Kubernetes " + strings.Join(versions, ", ") + ")"}
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
	if libvirt.DevKVMPresent() {
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

// checkSpark reports the Spark module (docs/design/09-spark.md section 2): off,
// on with no image (the import a cluster needs first), or on with the
// versions available. With object storage on it also warns about a guest S3
// endpoint no VPC can reach, which would refuse every cluster that asks for
// object storage.
func checkSpark(ctx context.Context, cfg *config.Config) check {
	const name = "spark"
	if !cfg.Spark {
		return check{name: name, ok: true, note: "off (set KUMO_SPARK=on to enable)"}
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "not checked: " + err.Error()}
	}
	defer db.Close() //nolint:errcheck // a failed close does not change the verdict

	rows, err := db.ListAvailableEngineImages(ctx)
	if err != nil {
		return check{name: name, ok: true, warn: true, note: "cannot read images: " + err.Error()}
	}
	var versions []string
	for _, row := range rows {
		if row.Engine.Valid && image.Engine(row.Engine.String) == image.EngineSpark && row.EngineVersion.Valid {
			versions = append(versions, row.EngineVersion.String)
		}
	}
	if len(versions) == 0 {
		return check{name: name, ok: true, warn: true,
			note: "on, but no Spark image is available: import one before creating a " +
				"cluster (see packaging/images/spark-4.3/README.md)"}
	}
	if cfg.ObjectStore != config.ObjectStoreNone && sparkEndpointUnroutable(cfg.SparkS3Endpoint) {
		return check{name: name, ok: true, warn: true,
			note: "on (Spark " + strings.Join(versions, ", ") + "), but KUMO_SPARK_S3_ENDPOINT " +
				"(" + cfg.SparkS3Endpoint + ") is not an address a VPC can reach: clusters with " +
				"object storage will be refused; set it to the host's LAN address"}
	}
	return check{name: name, ok: true, note: "on (Spark " + strings.Join(versions, ", ") + ")"}
}

// sparkEndpointUnroutable is the create's own check (internal/service/spark):
// empty, unparsable or loopback.
func sparkEndpointUnroutable(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if endpoint == "" || err != nil || u.Hostname() == "" || u.Hostname() == "localhost" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}
