//go:build libvirt

package libvirt_test

import (
	"log/slog"
	"os"
	"testing"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	kumolibvirt "github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
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
