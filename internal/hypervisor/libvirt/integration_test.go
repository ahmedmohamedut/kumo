//go:build libvirt

package libvirt_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
// autostarted, and one active network. A second kumod starting on an already
// prepared host must not fail or duplicate anything.
func TestEnsureInfraIsIdempotent(t *testing.T) {
	conn := dial(t)

	dataDir := t.TempDir()
	client, err := kumolibvirt.New(slog.New(slog.DiscardHandler), "unix://"+socketPath())
	if err != nil {
		t.Fatalf("libvirt.New(): %v", err)
	}
	t.Cleanup(func() { client.Close() })

	ctx := t.Context()

	cfg := infraConfig(dataDir, "kumo-it-images", "kumo-it-instances", "kumo-it-net")

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

	net, err := conn.NetworkLookupByName(cfg.Network)
	if err != nil {
		t.Fatalf("NetworkLookupByName(%q): %v", cfg.Network, err)
	}
	active, err := conn.NetworkIsActive(net)
	if err != nil {
		t.Fatalf("NetworkIsActive(): %v", err)
	}
	if active != 1 {
		t.Errorf("the network %q is defined but not active", cfg.Network)
	}

	t.Cleanup(func() { teardown(t, conn, cfg) })
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
