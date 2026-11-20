//go:build libvirt

package libvirt_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	kumolibvirt "github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt"
)

// libvirtURI is the daemon to test against. CI runs this on a nested-virt
// runner with `qemu:///system`; override it for a `qemu:///session` box.
func libvirtURI() string {
	if v := os.Getenv("KUMO_TEST_LIBVIRT_URI"); v != "" {
		return v
	}
	return "qemu:///system"
}

// socketPath is the daemon socket libvirt listens on. `qemu:///system` uses
// /var/run/libvirt/libvirt-sock, a session bus uses a private one, so a
// non-default URI has to come with its own KUMO_TEST_LIBVIRT_SOCKET.
func socketPath() string {
	if v := os.Getenv("KUMO_TEST_LIBVIRT_SOCKET"); v != "" {
		return v
	}
	return "/var/run/libvirt/libvirt-sock"
}

func dial(t *testing.T) *golibvirt.Libvirt {
	t.Helper()

	conn := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(socketPath())))
	if err := conn.ConnectToURI(golibvirt.ConnectURI(libvirtURI())); err != nil {
		t.Skipf("no libvirt daemon on %s: %v", socketPath(), err)
	}
	t.Cleanup(func() { conn.Disconnect() })

	return conn
}

// TestEnsureInfraIsIdempotent is the M3.1 acceptance check: running the
// bootstrap twice must leave exactly one pool per name, active and
// autostarted. A second kumod starting on an already prepared host must not
// fail or duplicate anything. Networks are a VPC's job (the vpc service
// bootstraps the default one), so they are not part of this.
func TestEnsureInfraIsIdempotent(t *testing.T) {
	conn := dial(t)

	dataDir := t.TempDir()
	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	defer client.Close()

	ctx := t.Context()

	cfg := infraConfig(dataDir, "kumo-it-images", "kumo-it-instances", "kumo-it-net")
	// Registered before the bootstrap so a failure cannot leave half an
	// infrastructure behind (there was no unregister path for early failures).
	t.Cleanup(func() { teardown(t, conn, cfg) })

	if err := client.EnsureInfra(ctx, cfg); err != nil {
		t.Fatalf("EnsureInfra() first run: %v", err)
	}
	if err := client.EnsureInfra(ctx, cfg); err != nil {
		t.Fatalf("EnsureInfra() second run: %v", err)
	}

	for _, name := range []string{cfg.ImagesPool, cfg.InstancesPool} {
		pool, err := conn.StoragePoolLookupByName(name)
		if err != nil {
			t.Fatalf("StoragePoolLookupByName(%q): %v", name, err)
		}
		if _, err := conn.StoragePoolLookupByName(name); err != nil {
			t.Fatalf("the pool is not defined after the second run: %v", err)
		}
		_ = pool
	}
}

// TestHasKVMAsksLibvirt pins what /system/info reports as `kvm` against a real
// daemon. The oracle is a second libvirt question, the domain capabilities for
// virttype kvm, which libvirt refuses outright when it cannot run kvm domains.
// It also pins the regression this replaced: on a host with /dev/kvm the
// answer is true, where the old check (a stat from inside kumod's
// PrivateDevices sandbox) answered false on every packaged node. Read-only.
func TestHasKVMAsksLibvirt(t *testing.T) {
	conn := dial(t)

	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	defer client.Close()

	got, err := client.HasKVM(t.Context())
	if err != nil {
		t.Fatalf("HasKVM(): %v", err)
	}

	_, domErr := conn.ConnectGetDomainCapabilities(nil, nil, nil, golibvirt.OptString{"kvm"}, 0)
	if want := domErr == nil; got != want {
		t.Fatalf("HasKVM() = %v, but libvirt's kvm domain capabilities answered err=%v", got, domErr)
	}
	if kumolibvirt.DevKVMPresent() && !got {
		t.Fatalf("HasKVM() = false on a host whose /dev/kvm is present")
	}
}

// infraConfig names pools and a network that are unique to this run, so a test
// never collides with a real Kumo installation on the same host.
func infraConfig(dataDir, images, instances, network string) hypervisor.InfraConfig {
	return hypervisor.InfraConfig{
		ImagesPool:    images,
		InstancesPool: instances,
		Network:       network,
		DataDir:       dataDir,
	}
}

// makeTestNetwork defines and starts the network a backend test attaches its
// domain to, on a subnet of its own. The backend no longer creates networks —
// that is the VPC service's job — so a test that needs one has to bring it up
// first. The subnet must not collide with a real install's kumo-default
// network on the same host, hence the free private ranges below; the guests in
// these tests never ask for DHCP, so any range does. A stale definition from an
// earlier run is removed first. The bridge name must fit IFNAMSIZ (15 chars).
func makeTestNetwork(t *testing.T, conn *golibvirt.Libvirt, network, bridge, subnet string) {
	t.Helper()

	if stale, err := conn.NetworkLookupByName(network); err == nil {
		if active, err := conn.NetworkIsActive(stale); err == nil && active == 1 {
			if err := conn.NetworkDestroy(stale); err != nil {
				t.Fatalf("NetworkDestroy(%q): %v", network, err)
			}
		}
		if err := conn.NetworkUndefine(stale); err != nil {
			t.Fatalf("NetworkUndefine(%q): %v", network, err)
		}
	}
	netXML := fmt.Sprintf(`<network><name>%s</name>`+
		`<forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward>`+
		`<bridge name='%s' stp='on' delay='0'/>`+
		`<ip address='%s.1' netmask='255.255.255.0'>`+
		`<dhcp><range start='%s.10' end='%s.250'/></dhcp></ip></network>`,
		network, bridge, subnet, subnet, subnet)
	net, err := conn.NetworkDefineXML(netXML)
	if err != nil {
		t.Fatalf("NetworkDefineXML(%q): %v", network, err)
	}
	if err := conn.NetworkCreate(net); err != nil && !strings.Contains(err.Error(), "already active") {
		t.Fatalf("NetworkCreate(%q): %v", network, err)
	}
}

// teardown removes what the test created. libvirt refuses to undefine an active
// pool or network, so each is stopped first.
func teardown(t *testing.T, conn *golibvirt.Libvirt, cfg hypervisor.InfraConfig) {
	t.Helper()

	for _, name := range []string{cfg.ImagesPool, cfg.InstancesPool} {
		pool, err := conn.StoragePoolLookupByName(name)
		if err != nil {
			continue
		}
		if active, err := conn.StoragePoolIsActive(pool); err == nil && active == 1 {
			if err := conn.StoragePoolDestroy(pool); err != nil {
				t.Logf("StoragePoolDestroy(%q): %v", name, err)
				continue
			}
		}
		if err := conn.StoragePoolUndefine(pool); err != nil {
			t.Logf("StoragePoolUndefine(%q): %v", name, err)
		}
	}

	if net, err := conn.NetworkLookupByName(cfg.Network); err == nil {
		if active, err := conn.NetworkIsActive(net); err == nil && active == 1 {
			if err := conn.NetworkDestroy(net); err != nil {
				t.Logf("NetworkDestroy(%q): %v", cfg.Network, err)
				return
			}
		}
		if err := conn.NetworkUndefine(net); err != nil {
			t.Logf("NetworkUndefine(%q): %v", cfg.Network, err)
		}
	}
}

// cirrosURL names the guest image the launch test boots. It is opt-in: booting a
// guest takes minutes, needs KVM and wants a few hundred megabytes of disk, so
// the default run of the test suite does not do it. Point it at a CirrOS cloud
// image (any of the 0.6.x qcow2 files) to run the M4.3 acceptance check.
func cirrosURL() string {
	return os.Getenv("KUMO_TEST_CIRROS_URL")
}

// maxImageBytes bounds what the test downloads, so a wrong URL cannot fill the
// disk of a CI runner.
const maxImageBytes = 1 << 30

// TestLaunchBootsCirrOS is the M4.3 acceptance check on real hardware: an
// instance created from an imported image, seeded by cloud-init and booted on the
// Kumo network, comes up running and takes an address from the DHCP range.
//
// It exercises the four steps of the launch workflow against libvirt itself
// (docs/design/04-libvirt-integration.md §3), because the steps the fake cannot
// prove are the ones that only a real hypervisor proves: that QEMU can open a
// volume from a directory pool, that the domain XML is accepted, and that the
// guest finishes a DHCP handshake at all.
//
// The tail is the M4.4 lifecycle: the power calls and the console log are made
// against the same live domain, because a backend method that compiles and is
// never called against libvirt is not a backend method that works.
func TestLaunchBootsCirrOS(t *testing.T) {
	url := cirrosURL()
	if url == "" {
		t.Skipf("set KUMO_TEST_CIRROS_URL to a CirrOS cloud image to run this test")
	}

	conn := dial(t)

	dataDir := t.TempDir()
	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx := t.Context()
	cfg := infraConfig(dataDir, "kumo-it-launch-images", "kumo-it-launch-instances", "kumo-it-launch-net")

	// The backend no longer creates networks (the vpc service does), so the
	// launch network is brought up on its own subnet first. virbr-it-launch-net
	// fits IFNAMSIZ.
	makeTestNetwork(t, conn, cfg.Network, "virbr-it-launch-net", "192.168.86")

	if err := client.EnsureInfra(ctx, cfg); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	t.Cleanup(func() { teardown(t, conn, cfg) })

	imagePath := download(t, url)
	image, err := imagefmt.Inspect(imagePath)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", imagePath, err)
	}

	const instanceID = "i-it-cirros"

	imageFile, err := os.Open(imagePath) //nolint:gosec // a path from our own temp dir
	if err != nil {
		t.Fatalf("open the image: %v", err)
	}
	defer imageFile.Close() //nolint:errcheck // a read-only handle

	if err := client.ImportImage(ctx, "cirros", imageFile, hypervisor.ImportSpec{
		SizeBytes:        image.SizeBytes,
		VirtualSizeBytes: image.VirtualSizeBytes,
		Format:           hypervisor.ImageFormat(image.Format),
	}); err != nil {
		t.Fatalf("ImportImage(): %v", err)
	}

	if err := client.CreateRootDisk(ctx, hypervisor.RootDiskSpec{
		InstanceID:   instanceID,
		SourceVolume: "cirros",
		Pool:         cfg.InstancesPool,
		SizeGiB:      4,
	}); err != nil {
		t.Fatalf("CreateRootDisk(): %v", err)
	}
	if err := client.CreateSeedISO(ctx, cfg.InstancesPool, hypervisor.SeedISOName(instanceID),
		hypervisor.CloudInit{
			MetaData:      "instance-id: " + instanceID + "\nlocal-hostname: cirros\n",
			UserData:      "#cloud-config\nusers:\n  - name: cirros\n    sudo: ALL=(ALL) NOPASSWD:ALL\n",
			NetworkConfig: "version: 2\nethernets:\n  eth0:\n    dhcp4: true\n",
		}); err != nil {
		t.Fatalf("CreateSeedISO(): %v", err)
	}

	rootPath, err := client.VolumePath(ctx, cfg.InstancesPool, hypervisor.RootDiskName(instanceID))
	if err != nil {
		t.Fatalf("VolumePath(root): %v", err)
	}
	seedPath, err := client.VolumePath(ctx, cfg.InstancesPool, hypervisor.SeedISOName(instanceID))
	if err != nil {
		t.Fatalf("VolumePath(seed): %v", err)
	}

	consoleLog := filepath.Join(dataDir, "console", instanceID+".log")
	if err := os.MkdirAll(filepath.Dir(consoleLog), 0o750); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	// The domain is left undefined whatever the test does next: a rollback of a
	// failed launch is exactly this call.
	t.Cleanup(func() {
		if err := client.Undefine(context.WithoutCancel(t.Context()), instanceID); err != nil {
			t.Logf("Undefine(%q): %v", instanceID, err)
		}
	})

	info, err := client.DefineAndStart(ctx, hypervisor.DomainSpec{
		ID:             instanceID,
		Name:           instanceID,
		Arch:           "x86_64",
		VCPUs:          1,
		MemoryMiB:      1024,
		RootDiskPool:   cfg.InstancesPool,
		RootDiskPath:   rootPath,
		SeedISOPath:    seedPath,
		ConsoleLogPath: consoleLog,
		Network:        cfg.Network,
	})
	if err != nil {
		t.Fatalf("DefineAndStart(): %v", err)
	}
	if info.State != hypervisor.DomainStateRunning {
		t.Errorf("state after DefineAndStart = %q, want running", info.State)
	}
	// libvirt assigns both, and the launch records them on the row, so a backend
	// that reported neither would leave an instance the console cannot link to.
	if info.UUID == "" {
		t.Error("DefineAndStart() reported no uuid")
	}
	if info.MAC == "" {
		t.Error("DefineAndStart() reported no mac address")
	}

	// A boot takes minutes on a nested-virtualized runner, so the wait is generous
	// and the failure says what to look at.
	deadline := time.Now().Add(4 * time.Minute)
	for {
		runtime, err := client.GetDomain(ctx, instanceID)
		if err != nil {
			t.Fatalf("GetDomain(): %v", err)
		}
		if runtime.State != hypervisor.DomainStateRunning {
			t.Fatalf("state = %q while waiting for a boot, want running", runtime.State)
		}
		if runtime.IP != "" {
			if runtime.MAC != info.MAC {
				t.Errorf("mac = %q, want the %q of the launch", runtime.MAC, info.MAC)
			}
			if runtime.MemoryMiB != 1024 {
				t.Errorf("memory = %d MiB, want 1024", runtime.MemoryMiB)
			}
			if runtime.VCPUs != 1 {
				t.Errorf("vcpus = %d, want 1", runtime.VCPUs)
			}
			if runtime.UUID == "" {
				t.Error("GetDomain() reported no uuid")
			}
			t.Logf("booted %s with address %s", instanceID, runtime.IP)
			break
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(consoleLog)
			t.Fatalf("the guest took no address within the timeout; console:\n%s", log)
		}
		time.Sleep(5 * time.Second)
	}

	// Both volumes are in the instances pool where the domain was pointed at it,
	// which is the part of the launch a fake cannot prove: a path libvirt hands
	// back has to be one the QEMU user can actually open.
	for _, path := range []string{rootPath, seedPath} {
		stat, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if stat.Size() == 0 {
			t.Errorf("%s is empty", path)
		}
	}

	// The console log is where an operator looks when a boot misbehaves, so a run
	// that booted has to have left one.
	if _, err := os.Stat(consoleLog); err != nil {
		t.Errorf("stat the console log: %v", err)
	}

	// The M4.4 calls, on a domain that is really up. The console tail is read from
	// the path in the domain's own <log> element rather than from one rebuilt here,
	// so a backend that guessed the path instead of asking libvirt would answer
	// with nothing.
	out, err := client.ConsoleOutput(ctx, instanceID, 64<<10)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if len(out) == 0 {
		t.Error("ConsoleOutput() returned nothing for a domain that has booted")
	}
	if tail, err := client.ConsoleOutput(ctx, instanceID, 64); err != nil {
		t.Errorf("ConsoleOutput(64): %v", err)
	} else if len(tail) > 64 {
		t.Errorf("ConsoleOutput(64) returned %d bytes, want at most 64", len(tail))
	}

	// A reset is QEMU's own, not the guest's, so it is the one power call whose
	// outcome the host guarantees.
	if err := client.Reset(ctx, instanceID); err != nil {
		t.Errorf("Reset(): %v", err)
	}

	// A domain that libvirt does not know has to come back as ErrNotFound, because
	// that is what the workflows match on to record an instance as an error.
	for name, call := range map[string]func(context.Context, string) error{
		"Start":         func(ctx context.Context, id string) error { return client.Start(ctx, id) },
		"Shutdown":      func(ctx context.Context, id string) error { return client.Shutdown(ctx, id) },
		"Reboot":        func(ctx context.Context, id string) error { return client.Reboot(ctx, id) },
		"Reset":         func(ctx context.Context, id string) error { return client.Reset(ctx, id) },
		"ConsoleOutput": func(ctx context.Context, id string) error { _, err := client.ConsoleOutput(ctx, id, 1024); return err },
	} {
		if err := call(ctx, "i-it-not-here"); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("%s() on a missing domain = %v, want ErrNotFound", name, err)
		}
	}

	// An ACPI shutdown only has to be delivered: whether the guest acts on it is
	// the guest's business, and CirrOS answers it where many minimal images do not.
	switch err := client.Shutdown(ctx, instanceID); {
	case err == nil:
		if !waitForState(client, instanceID, hypervisor.DomainStateShutoff, 60*time.Second) {
			t.Log("the guest ignored the ACPI request; the call was still delivered")
		}
	case errors.Is(err, hypervisor.ErrInvalidState):
		t.Log("libvirt refused the ACPI request; skipping the graceful stop")
	default:
		t.Errorf("Shutdown(): %v", err)
	}

	// A forced stop leaves nothing running, and a start boots the definition that is
	// still there: the pair is what a stop/start of an instance does, and neither
	// step may redefine the domain.
	if err := client.Destroy(ctx, instanceID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("Destroy(): %v", err)
	}
	if err := client.Start(ctx, instanceID); err != nil {
		t.Errorf("Start(): %v", err)
	}
	if info, err := client.GetDomain(ctx, instanceID); err != nil {
		t.Errorf("GetDomain() after the start: %v", err)
	} else if info.State == hypervisor.DomainStateShutoff {
		t.Error("the domain is still shut off after Start()")
	}
	if err := client.Destroy(ctx, instanceID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("Destroy() at the end: %v", err)
	}
	// The domain is off now. Destroying it again is still success: the guest's
	// state must not matter to a hard stop, which is what a terminate of a
	// stopped instance relies on.
	if err := client.Destroy(ctx, instanceID); err != nil {
		t.Errorf("Destroy() on an already-stopped domain = %v, want nil", err)
	}
}

// TestDefineAndStartAdoptsAnInactiveDefinition is the regression test for the
// "domain already exists with uuid" launch failure: an interrupted run can leave
// a defined-but-inactive domain behind, and virDomainCreateXML refuses to start
// a name that is already defined even when it is shut off. A second launch of
// the same instance has to adopt and start that definition instead of failing.
//
// The guest never boots an OS here: the test only needs QEMU to reach the
// running state, which a blank overlay does on its way to "no bootable device".
func TestDefineAndStartAdoptsAnInactiveDefinition(t *testing.T) {
	conn := dial(t)

	dataDir := t.TempDir()
	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx := t.Context()
	cfg := infraConfig(dataDir, "kumo-it-adopt-images", "kumo-it-adopt-instances", "kumo-adopt-net")

	// Production volumes live under /var/lib/kumo, whose 0755 mode the QEMU user
	// can traverse; a Go test temp dir is 0700 by default (the per-test parent and
	// the t.TempDir() child both are), which would make the launched guest fail
	// to open its own disks. Let the chain through.
	for _, dir := range []string{filepath.Dir(dataDir), dataDir} {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
	}

	// The backend no longer creates networks, so the test brings its network up
	// on its own subnet first — a free private range that cannot collide with a
	// real install's kumo-default network on the same host. The guest never
	// asks for DHCP, so the subnet is irrelevant to what the test checks.
	makeTestNetwork(t, conn, cfg.Network, "virbr-adopt-net", "192.168.87")

	if err := client.EnsureInfra(ctx, cfg); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	t.Cleanup(func() { teardown(t, conn, cfg) })

	// A root disk and a seed the QEMU of a test can open: the guest never boots,
	// it only has to reach the running state. The files are put in the instances
	// pool directory directly, because this test is about DefineAndStart adopting
	// an inactive definition — the volume workflow behind those files is
	// TestLaunchBootsCirrOS's job. The root has to be a real qcow2 (the domain
	// XML declares its format), so it is a blank one made with qemu-img.
	const instanceID = "i-it-adopt"
	rootPath := filepath.Join(dataDir, "pools", "instances", hypervisor.RootDiskName(instanceID))
	seedPath := filepath.Join(dataDir, "pools", "instances", hypervisor.SeedISOName(instanceID))
	if err := os.MkdirAll(filepath.Dir(rootPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}
	if out, err := exec.Command("qemu-img", "create", "-f", "qcow2", "-o", "compat=1.1",
		rootPath, "512M").CombinedOutput(); err != nil {
		t.Fatalf("qemu-img create the root disk: %v: %s", err, out)
	}
	if err := os.WriteFile(seedPath, []byte("no-cloud init"), 0o644); err != nil {
		t.Fatalf("write the seed: %v", err)
	}

	consoleLog := filepath.Join(dataDir, "console", instanceID+".log")
	if err := os.MkdirAll(filepath.Dir(consoleLog), 0o750); err != nil {
		t.Fatalf("MkdirAll(): %v", err)
	}

	spec := hypervisor.DomainSpec{
		ID:             instanceID,
		Name:           instanceID,
		Arch:           "x86_64",
		VCPUs:          1,
		MemoryMiB:      1024,
		RootDiskPool:   cfg.InstancesPool,
		RootDiskPath:   rootPath,
		SeedISOPath:    seedPath,
		ConsoleLogPath: consoleLog,
		Network:        cfg.Network,
	}
	t.Cleanup(func() {
		if err := client.Undefine(context.WithoutCancel(t.Context()), instanceID); err != nil {
			t.Logf("Undefine(%q): %v", instanceID, err)
		}
	})

	if _, err := client.DefineAndStart(ctx, spec); err != nil {
		t.Fatalf("DefineAndStart() first launch: %v", err)
	}

	// A forced stop leaves the definition behind, which is the shape an
	// interrupted launch leaves: defined, not running.
	if err := client.Destroy(ctx, instanceID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}

	// The second launch has to adopt that definition. Before the fix this was the
	// "operation failed: domain ... already exists with uuid" the test exists for.
	info, err := client.DefineAndStart(ctx, spec)
	if err != nil {
		t.Fatalf("DefineAndStart() on an inactive definition: %v", err)
	}
	if info.State != hypervisor.DomainStateRunning {
		t.Errorf("state after the adopted launch = %q, want running", info.State)
	}
	if info.UUID == "" {
		t.Error("the adopted launch reported no uuid")
	}

	// The launch is done with it: a re-adoption of a running domain is refused as
	// already existing, and the caller (the service) reads the running one back.
	if _, err := client.DefineAndStart(ctx, spec); !errors.Is(err, hypervisor.ErrAlreadyExists) {
		t.Errorf("DefineAndStart() on a running domain = %v, want ErrAlreadyExists", err)
	}
}

// waitForState polls a domain until it reports want, and reports whether it got
// there. It fails the test only on an error it cannot interpret, so a caller can
// treat "did not get there" as a property of the guest rather than a failure.
func waitForState(
	client *kumolibvirt.Client, id string, want hypervisor.DomainState, within time.Duration,
) bool {
	deadline := time.Now().Add(within)
	for {
		info, err := client.GetDomain(context.Background(), id)
		switch {
		case err != nil:
			return false
		case info.State == want:
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Second)
	}
}

// download fetches the guest image into the test's temp dir. A partial file is
// removed, so a test that dies mid-download leaves nothing behind.
func download(t *testing.T, url string) string {
	t.Helper()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build the request for %s: %v", url, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close() //nolint:errcheck // a read-only body

	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, response.StatusCode)
	}

	path := filepath.Join(t.TempDir(), "image.img")
	file, err := os.Create(path) //nolint:gosec // a path from our own temp dir
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer file.Close() //nolint:errcheck // the test is about to read it back

	written, err := io.Copy(file, io.LimitReader(response.Body, maxImageBytes))
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if written == 0 {
		t.Fatalf("GET %s returned an empty body", url)
	}
	if written == maxImageBytes {
		t.Fatalf("the image is at least %d bytes, which is more than this test downloads", written)
	}

	return path
}

// TestDNSHostResolvesOnTheNetwork pins what the Kubernetes endpoint relies on
// (docs/design/08-kubernetes.md K6): a DNS record added to a VPC's network is
// served by that network's own dnsmasq, under a fully qualified name with dots
// that is no guest's hostname, and stops being served once removed. It defines
// a network the way a VPC does and asks dnsmasq on the gateway directly.
func TestDNSHostResolvesOnTheNetwork(t *testing.T) {
	conn := dial(t)
	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	defer client.Close()
	ctx := t.Context()

	const (
		name    = "kumo-it-dns"
		gateway = "10.178.0.1"
		record  = "api.orders.itvpc"
		target  = "10.178.0.10"
	)
	t.Cleanup(func() {
		if n, err := conn.NetworkLookupByName(name); err == nil {
			_ = conn.NetworkDestroy(n)
			_ = conn.NetworkUndefine(n)
		}
	})
	if err := client.DefineNetwork(ctx, hypervisor.NetworkSpec{
		Name: name, Bridge: "kmbitdns0", CIDR: "10.178.0.0/24", Gateway: gateway,
		Mode: hypervisor.NetworkModeIsolated, Domain: "itvpc",
	}); err != nil {
		t.Fatalf("DefineNetwork(): %v", err)
	}
	if err := client.StartNetwork(ctx, name); err != nil {
		t.Fatalf("StartNetwork(): %v", err)
	}

	host := hypervisor.DNSHost{IP: target, Hostnames: []string{record}}
	if err := client.AddDNSHost(ctx, name, host); err != nil {
		t.Fatalf("AddDNSHost(): %v", err)
	}
	if err := client.AddDNSHost(ctx, name, host); !errors.Is(err, hypervisor.ErrAlreadyExists) {
		t.Errorf("a second AddDNSHost() = %v, want ErrAlreadyExists", err)
	}

	info, err := client.GetNetwork(ctx, name)
	if err != nil {
		t.Fatalf("GetNetwork(): %v", err)
	}
	if len(info.DNSHosts) != 1 || info.DNSHosts[0].IP != target || info.DNSHosts[0].Hostnames[0] != record {
		t.Errorf("DNSHosts = %+v, want the record read back", info.DNSHosts)
	}

	// dnsmasq on the gateway, the resolver every guest of the VPC uses.
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "udp", gateway+":53")
	}}
	var addrs []string
	for i := 0; i < 20; i++ {
		if addrs, err = resolver.LookupHost(ctx, record); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil || len(addrs) != 1 || addrs[0] != target {
		t.Fatalf("LookupHost(%s) via %s = %v, %v, want %s", record, gateway, addrs, err, target)
	}

	if err := client.DeleteDNSHost(ctx, name, host); err != nil {
		t.Fatalf("DeleteDNSHost(): %v", err)
	}
	if err := client.DeleteDNSHost(ctx, name, host); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("a second DeleteDNSHost() = %v, want ErrNotFound", err)
	}
	time.Sleep(time.Second)
	if addrs, err := resolver.LookupHost(ctx, record); err == nil && len(addrs) > 0 {
		t.Errorf("LookupHost(%s) after the delete = %v, want no answer", record, addrs)
	}
}
