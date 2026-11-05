//go:build libvirt

package dbinstance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket/dialers"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	kumolibvirt "github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/service/vpc"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// TestDatabaseJourney is the M7.7 acceptance check on real hardware: a DB
// instance created through the real services on a real libvirtd, from the
// golden image of M7.1, with a real Postgres answering inside the guest.
//
// What only a real node can prove, and the fake cannot:
//
//   - the first-boot script of §5 runs to the end: the data disk is formatted
//     and mounted, the cluster is created on it, the role and the database
//     exist and the engine opens the VPC address last;
//   - the **original cleartext password** authenticates against the role that
//     was created from the verifier Kumo pre-computed and discarded, over
//     SCRAM-SHA-256, and the server's signature checks out (§5, R8);
//   - `pg_hba.conf` admits the VPC and nothing else, which is the whole of the
//     security boundary (R6);
//   - the endpoint resolves in the VPC's DNS, because dnsmasq serves the
//     static host Kumo wrote (R9);
//   - stop, start and reboot keep the endpoint and the data;
//   - a delete takes the data disk with it.
//
// It is opt-in, like every test that needs a daemon: point
// KUMO_TEST_ENGINE_IMAGE at the qcow2 that packaging/images/postgres-18/
// builds. The engine image is served over loopback so the import runs through
// the real `image.import` task, download and all, rather than a volume written
// behind the service's back.
func TestDatabaseJourney(t *testing.T) {
	imagePath := os.Getenv("KUMO_TEST_ENGINE_IMAGE")
	if imagePath == "" {
		t.Skip("set KUMO_TEST_ENGINE_IMAGE to the qcow2 from packaging/images/postgres-18 to run this")
	}
	if _, err := os.Stat(imagePath); err != nil {
		t.Fatalf("KUMO_TEST_ENGINE_IMAGE: %v", err)
	}

	h := newNode(t)
	engine := h.importEngineImage(t, imagePath)

	// The password is deliberately hostile: a quote, a double quote, a dollar,
	// a backslash, a space and an ampersand, so the whole quoting chain from
	// the request through the shell and into psql's heredoc is covered rather
	// than assumed (§5).
	const adminPassword = `Tr0ub4dor&3 'quote"$dollar\back`
	const identifier = "it-orders"

	created, err := h.databases.Create(t.Context(), dbinstance.CreateInput{
		Identifier:    identifier,
		Engine:        dbinstance.EnginePostgres,
		EngineVersion: "18",
		InstanceType:  "kumo.small",
		StorageGB:     8,
		AdminUsername: "app",
		AdminPassword: adminPassword,
		DatabaseName:  "orders",
	})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Logf("creating %s (server %s) from %s", created.ID, created.InstanceID, engine.ID)

	// Whatever the test does next, the database and its storage go: a leftover
	// guest would hold the node's memory and a leftover data disk its disk.
	t.Cleanup(func() { h.deleteDatabase(t, created.ID) })

	ready := h.awaitState(t, created.ID, dbinstance.StateAvailable, 8*time.Minute)
	if ready.Endpoint == nil {
		t.Fatal("the DB instance is available with no endpoint")
	}
	t.Logf("available at %s (%s)", *ready.Endpoint, h.addressOf(t, ready.InstanceID))

	address := h.addressOf(t, ready.InstanceID)
	port := strconv.Itoa(ready.Port)

	t.Run("the endpoint is the identifier under the VPC's DNS name", func(t *testing.T) {
		if want := identifier + "." + h.vpcName; *ready.Endpoint != want {
			t.Errorf("endpoint = %q, want %q", *ready.Endpoint, want)
		}
	})

	t.Run("the VPC's DNS resolves the endpoint to the guest", func(t *testing.T) {
		// dnsmasq serves the static host Kumo wrote, which is what makes the
		// endpoint a name a client in the VPC can use (R9). The query goes to
		// the network's own resolver on the gateway, because the host does not
		// use it.
		resolved := h.resolveInVPC(t, []string{*ready.Endpoint, identifier})
		if resolved != address {
			t.Errorf("the VPC's DNS resolves the endpoint to %q, want the guest's %q",
				resolved, address)
		}
	})

	t.Run("the guest carries both disks and the data disk by serial", func(t *testing.T) {
		h.assertDisks(t, ready)
	})

	t.Run("the original password authenticates over SCRAM", func(t *testing.T) {
		// The whole point of §5: Kumo computed a verifier, put it in the seed
		// and threw the password away, and the password still works. The
		// server's signature is checked too, so this is evidence that both
		// sides derived from the same secret rather than that the server was
		// willing to say yes.
		conn, err := pgDial(net.JoinHostPort(address, port), nil, 15*time.Second)
		if err != nil {
			t.Fatalf("dial the engine: %v", err)
		}
		defer conn.Close()

		if err := conn.Login(ready.AdminUsername, ready.DatabaseName, adminPassword); err != nil {
			t.Fatalf("login as %s: %v", ready.AdminUsername, err)
		}
	})

	t.Run("a wrong password is refused", func(t *testing.T) {
		conn, err := pgDial(net.JoinHostPort(address, port), nil, 15*time.Second)
		if err != nil {
			t.Fatalf("dial the engine: %v", err)
		}
		defer conn.Close()

		err = conn.Login(ready.AdminUsername, ready.DatabaseName, adminPassword+"x")
		var refusal *pgError
		if !errors.As(err, &refusal) {
			t.Fatalf("login with a wrong password: error = %v, want a refusal", err)
		}
		// 28P01 is invalid_password: the engine is checking the proof rather
		// than trusting the client.
		if refusal.Code != "28P01" {
			t.Errorf("refusal code = %q (%s), want 28P01", refusal.Code, refusal.Message)
		}
	})

	t.Run("a client outside the VPC cannot connect", func(t *testing.T) {
		// R6 is one line of pg_hba.conf, and this is the only way to see it
		// from outside: present an address the guest's VPC does not hold. The
		// host's own LAN address is one, and the guest can route a reply to it
		// through its gateway, so the connection reaches the engine and is
		// refused there.
		outside := h.hostAddressOutside(t)
		if outside == nil {
			t.Skip("no host address outside the VPC to present as a foreign client")
		}

		conn, err := pgDial(net.JoinHostPort(address, port), outside, 10*time.Second)
		if err != nil {
			// Not reachable at all is the stronger outcome, and the one a node
			// with stricter forwarding gives: nothing outside the VPC can even
			// open the port.
			t.Logf("a foreign client cannot reach the port at all: %v", err)
			return
		}
		defer conn.Close()

		err = conn.Login(ready.AdminUsername, ready.DatabaseName, adminPassword)
		var refusal *pgError
		if !errors.As(err, &refusal) {
			t.Fatalf("a client from %s was not refused: error = %v", outside, err)
		}
		if !strings.Contains(refusal.Message, "pg_hba.conf") {
			t.Errorf("refusal = %q, want it to name pg_hba.conf", refusal.Message)
		}
		t.Logf("a client from %s is refused: %s", outside, refusal.Message)
	})

	t.Run("a stop and a start keep the endpoint and the data", func(t *testing.T) {
		if _, err := h.databases.Stop(t.Context(), created.ID); err != nil {
			t.Fatalf("Stop(): %v", err)
		}
		stopped := h.awaitState(t, created.ID, dbinstance.StateStopped, 3*time.Minute)
		if stopped.Endpoint == nil || *stopped.Endpoint != *ready.Endpoint {
			t.Errorf("endpoint = %v after a stop, want %q", stopped.Endpoint, *ready.Endpoint)
		}

		if _, err := h.databases.Start(t.Context(), created.ID); err != nil {
			t.Fatalf("Start(): %v", err)
		}
		started := h.awaitState(t, created.ID, dbinstance.StateAvailable, 5*time.Minute)
		if started.Endpoint == nil || *started.Endpoint != *ready.Endpoint {
			t.Errorf("endpoint = %v after a start, want %q", started.Endpoint, *ready.Endpoint)
		}
		if got := h.addressOf(t, started.InstanceID); got != address {
			t.Errorf("address = %q after a start, want the same %q", got, address)
		}

		// The cluster survived the power cycle, which is the data disk doing
		// its job: the role and the database are still there.
		conn, err := pgDial(net.JoinHostPort(address, port), nil, 20*time.Second)
		if err != nil {
			t.Fatalf("dial the engine after a start: %v", err)
		}
		defer conn.Close()
		if err := conn.Login(started.AdminUsername, started.DatabaseName, adminPassword); err != nil {
			t.Fatalf("login after a start: %v", err)
		}
	})

	t.Run("a reboot leaves it available", func(t *testing.T) {
		if _, err := h.databases.Reboot(t.Context(), created.ID); err != nil {
			t.Fatalf("Reboot(): %v", err)
		}
		// The reconciler is what publishes it again, so this waits for the
		// pass rather than for a task.
		rebooted := h.awaitState(t, created.ID, dbinstance.StateAvailable, 5*time.Minute)
		if rebooted.Endpoint == nil || *rebooted.Endpoint != *ready.Endpoint {
			t.Errorf("endpoint = %v after a reboot, want %q", rebooted.Endpoint, *ready.Endpoint)
		}
	})

	t.Run("neither the password nor the verifier is in the log", func(t *testing.T) {
		// The create, the seed rendering, both lifecycle actions and the
		// reconcile passes have all run by now, so the log holds everything
		// kumod would have written about this database.
		logged := h.logs()
		if strings.Contains(logged, adminPassword) {
			t.Error("the log carries the admin password")
		}
		if strings.Contains(logged, "SCRAM-SHA-256$") {
			t.Error("the log carries a SCRAM verifier")
		}
		// And the task payload, which is a row anybody with the database can
		// read, carries only the DB instance id.
		h.assertTaskPayloadsClean(t, created.ID, adminPassword)
	})

	t.Run("a delete takes the data disk with it", func(t *testing.T) {
		h.deleteDatabase(t, created.ID)

		if _, err := h.databases.Get(t.Context(), created.ID); err == nil {
			t.Error("the DB instance survived the delete")
		}
		if _, err := h.store.GetInstance(t.Context(),
			sqlc.GetInstanceParams{ID: created.InstanceID}); err == nil {
			t.Error("the server row survived the delete")
		}
		for _, name := range []string{
			hypervisor.DataDiskName(created.ID),
			hypervisor.RootDiskName(created.InstanceID),
			hypervisor.SeedISOName(created.InstanceID),
		} {
			if _, err := h.hv.VolumePath(t.Context(), h.instancesPool, name); !errors.Is(
				err, hypervisor.ErrNotFound) {
				t.Errorf("the volume %q survived the delete (err = %v)", name, err)
			}
		}
		if _, err := h.hv.GetDomain(t.Context(), created.InstanceID); !errors.Is(
			err, hypervisor.ErrNotFound) {
			t.Errorf("the domain survived the delete (err = %v)", err)
		}
	})
}

// node is the control plane of one test: the real services over a real
// libvirtd, wired the way internal/cli/serve.go wires them.
type node struct {
	store         *store.Store
	hv            *kumolibvirt.Client
	images        *image.Service
	instances     *instance.Service
	databases     *dbinstance.Service
	vpcs          *vpc.Service
	subnets       *subnet.Service
	instancesPool string
	imagesPool    string
	vpcID         string
	vpcName       string
	vpcCIDR       string
	logBuffer     *syncBuffer
}

// newNode brings up the services, the pools and a VPC of its own.
func newNode(t *testing.T) *node {
	t.Helper()

	socket := os.Getenv("KUMO_TEST_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/var/run/libvirt/libvirt-sock"
	}

	logBuffer := &syncBuffer{}
	// Everything the services log goes into the buffer, so the test can read
	// back exactly what kumod would have written (and prove what it did not).
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	hv, err := kumolibvirt.New(logger, "unix://"+socket)
	if err != nil {
		t.Skipf("no libvirt at %s: %v", socket, err)
	}
	if err := hv.Ping(t.Context()); err != nil {
		t.Skipf("libvirt at %s does not answer: %v", socket, err)
	}
	t.Cleanup(func() { _ = hv.Close() })

	// The data directory holds the pools, so it must not be on tmpfs: the
	// engine image alone is two gigabytes. KUMO_TEST_DATA_DIR is where a node
	// with a small /tmp points it.
	dataDir := os.Getenv("KUMO_TEST_DATA_DIR")
	if dataDir == "" {
		dataDir = t.TempDir()
	} else {
		dataDir = filepath.Join(dataDir, "kumo-it-db")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dataDir, err)
		}
	}
	// qemu opens the volumes as its own user, so every directory above them
	// has to be traversable by it.
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("Chmod(%s): %v", dataDir, err)
	}

	const (
		imagesPool    = "kumo-it-db-images"
		instancesPool = "kumo-it-db-instances"
	)
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool:    imagesPool,
		InstancesPool: instancesPool,
		Network:       "kumo-it-db-net",
		DataDir:       dataDir,
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	// The pools are left defined on purpose: EnsureInfra adopts them on the
	// next run, and removing a pool whose directory the test still owns is
	// more cleanup than it is worth. Every volume this test makes is deleted.

	db := storetest.New(t)
	subnetLocks := locks.New()

	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	subnets, err := subnet.New(subnet.Options{Logger: logger, Store: db, Locks: subnetLocks})
	if err != nil {
		t.Fatalf("subnet.New(): %v", err)
	}

	// A range of its own, so the test cannot collide with the host's LAN or
	// with libvirt's default network.
	vpcCIDR := os.Getenv("KUMO_TEST_VPC_CIDR")
	if vpcCIDR == "" {
		vpcCIDR = "10.177.0.0/24"
	}
	vpcs, err := vpc.New(vpc.Options{
		Logger: logger, Store: db, Hypervisor: hv, Tasks: tasks, Subnets: subnets,
		DefaultVPCCIDR: vpcCIDR,
	})
	if err != nil {
		t.Fatalf("vpc.New(): %v", err)
	}

	types, err := instancetype.New(instancetype.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	pairs, err := keypair.New(keypair.Options{Logger: logger, Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}
	staging := filepath.Join(dataDir, "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", staging, err)
	}
	images, err := image.New(image.Options{
		Logger:     logger,
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		// The engine image is served over loopback, which is what the
		// private-network allowance exists for (the same case
		// KUMO_IMAGE_ALLOW_PRIVATE_URLS covers on a node).
		Downloader: imagefetch.New(imagefetch.Options{
			MaxBytes:             8 << 30,
			AllowPrivateNetworks: true,
		}),
		ImagesPool: imagesPool,
		TempDir:    staging,
		MaxBytes:   8 << 30,
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}

	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys := system.New(logger, "test", "libvirt", db, hv, db)
	sys.SetLimits(limits)

	// The default VPC is created through the service, so the network, the
	// bridge and dnsmasq are the ones a node would have.
	defaultVPC, err := vpcs.Bootstrap(t.Context(), "kumo-it-db-net")
	if err != nil {
		t.Fatalf("vpc.Bootstrap(): %v", err)
	}
	t.Logf("VPC %s (%s) on %s", defaultVPC.ID, defaultVPC.Name, defaultVPC.CIDR)

	instances, err := instance.New(instance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks,
		Images: images, Types: types, KeyPairs: pairs, Capacity: sys,
		InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: filepath.Join(dataDir, "console"),
		// The network the default VPC brought up, which is where every guest
		// of this test sits.
		Network:     defaultVPC.LibvirtNetwork,
		SubnetLocks: subnetLocks, Limits: limits,
		// A first boot of the golden image is slower than a CirrOS boot.
		PollInterval: 2 * time.Second,
		PollTimeout:  3 * time.Minute,
		StopTimeout:  2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}

	databases, err := dbinstance.New(dbinstance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
		InstancesPool: instancesPool,
		ProbeInterval: 3 * time.Second,
		ReadyTimeout:  6 * time.Minute,
	})
	if err != nil {
		t.Fatalf("dbinstance.New(): %v", err)
	}

	out := &node{
		store:         db,
		hv:            hv,
		images:        images,
		instances:     instances,
		databases:     databases,
		vpcs:          vpcs,
		subnets:       subnets,
		instancesPool: instancesPool,
		imagesPool:    imagesPool,
		vpcID:         defaultVPC.ID,
		vpcName:       defaultVPC.Name,
		vpcCIDR:       defaultVPC.CIDR,
		logBuffer:     logBuffer,
	}

	// One worker pool over the real registry, and the reconciler, so the
	// workflows run exactly as they do under kumod.
	//
	// They run under a context of the test's own rather than t.Context(),
	// which Go cancels *before* the cleanups: the cleanups queue a delete for
	// the database, the image and the VPC, and a stopped worker would leave
	// every one of them in the pool and the network behind for the next run to
	// collide with.
	runCtx, stopRun := context.WithCancel(context.Background())
	pool := worker.New(worker.Options{
		Logger: logger, Store: db, Workers: 2, PollInterval: 200 * time.Millisecond,
		Registry: mergeRegistries(images.Handler(), instances.Handler(),
			vpcs.Handler(), databases.Handler()),
	})
	if err := pool.Start(runCtx); err != nil {
		stopRun()
		t.Fatalf("worker.Start(): %v", err)
	}

	reconciler, err := worker.NewReconciler(worker.ReconcilerOptions{
		Logger:   logger,
		Interval: 3 * time.Second,
		Pass: func(ctx context.Context) error {
			if _, err := instances.Reconcile(ctx); err != nil {
				return err
			}
			_, err := databases.Reconcile(ctx)
			return err
		},
		Hint: instances.ReconcileInstance,
	})
	if err != nil {
		t.Fatalf("worker.NewReconciler(): %v", err)
	}
	if err := reconciler.Start(runCtx); err != nil {
		stopRun()
		t.Fatalf("reconciler.Start(): %v", err)
	}

	// The network goes last, after the guests that sat on it. Its subnet goes
	// first: a VPC with subnets is refused, which is the same order an
	// operator follows.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())

		if row, err := db.GetDefaultSubnet(ctx); err == nil {
			if err := out.subnets.Delete(ctx, row.ID); err != nil {
				t.Logf("delete the default subnet: %v", err)
			}
		}
		if _, err := vpcs.Delete(ctx, out.vpcID); err != nil {
			t.Logf("delete the VPC: %v", err)
		} else {
			deadline := time.Now().Add(2 * time.Minute)
			for time.Now().Before(deadline) {
				if _, err := vpcs.Get(ctx, out.vpcID); err != nil {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
		}

		stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := reconciler.Stop(stopCtx); err != nil {
			t.Logf("stop the reconciler: %v", err)
		}
		if err := pool.Stop(stopCtx); err != nil {
			t.Logf("stop the worker: %v", err)
		}
		stopRun()
	})

	return out
}

// mergeRegistries is internal/cli/serve.go's mergeHandlers, which is not
// exported.
func mergeRegistries(registries ...worker.Registry) worker.Registry {
	merged := worker.Registry{}
	for _, registry := range registries {
		for taskType, handler := range registry {
			merged[taskType] = handler
		}
	}
	return merged
}

// importEngineImage serves the golden image over loopback and imports it the
// way an operator would: a URL, a checksum and the engine fields of M7.2.
func (h *node) importEngineImage(t *testing.T, path string) image.Imported {
	t.Helper()

	info, err := imagefmt.Inspect(path)
	if err != nil {
		t.Fatalf("Inspect(%s): %v", path, err)
	}

	server := httpFileServer(t, path)
	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name:          "kumo-it-postgres-18",
		SourceURL:     server + "/engine.qcow2",
		OSFamily:      "linux",
		Architecture:  "x86_64",
		DefaultUser:   "ubuntu",
		MinDiskGB:     int(info.VirtualSizeBytes >> 30),
		Kind:          image.KindEngine,
		Engine:        image.EnginePostgres,
		EngineVersion: "18",
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}

	// The image volume is deleted at the end: two gigabytes in a pool is not
	// something to leave behind on a developer's machine.
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := h.images.Delete(ctx, imported.ID); err != nil {
			t.Logf("delete the engine image: %v", err)
			return
		}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := h.images.Get(ctx, imported.ID); err != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Log("the engine image was still there after the delete")
	})

	deadline := time.Now().Add(10 * time.Minute)
	for {
		got, err := h.images.Get(t.Context(), imported.ID)
		if err != nil {
			t.Fatalf("Get(%q): %v", imported.ID, err)
		}
		switch got.State {
		case image.StateAvailable:
			t.Logf("engine image %s available (%d GiB min disk)", got.ID, got.MinDiskGB)
			return imported
		case image.StateFailed:
			t.Fatalf("the engine image import failed: %v", got.StateReason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the engine image is %s after 10 minutes", got.State)
		}
		time.Sleep(time.Second)
	}
}

// httpFileServer serves one file on loopback and returns its base URL.
func httpFileServer(t *testing.T, path string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen(): %v", err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			file, err := os.Open(path) //nolint:gosec // the path the test was given
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			defer file.Close() //nolint:errcheck // a read-only handle
			info, err := file.Stat()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
			w.Header().Set("Content-Type", "application/octet-stream")
			if r.Method == http.MethodHead {
				return
			}
			_, _ = io.Copy(w, file)
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})

	return "http://" + listener.Addr().String()
}

// awaitState waits for a DB instance to reach one state, failing with the
// reason it reached another.
func (h *node) awaitState(
	t *testing.T, dbInstanceID string, want dbinstance.State, timeout time.Duration,
) dbinstance.DBInstance {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var last dbinstance.DBInstance
	for {
		got, err := h.databases.Get(t.Context(), dbInstanceID)
		if err != nil {
			t.Fatalf("Get(%q): %v", dbInstanceID, err)
		}
		last = got
		if got.State == want {
			return got
		}
		if got.State == dbinstance.StateFailed && want != dbinstance.StateFailed {
			t.Logf("service log:\n%s", h.logTail(60))
			t.Fatalf("the DB instance failed: %s", reasonOf(got))
		}
		if time.Now().After(deadline) {
			t.Logf("service log:\n%s", h.logTail(60))
			t.Fatalf("the DB instance is %s after %s, want %s (reason %s)",
				last.State, timeout, want, reasonOf(last))
		}
		time.Sleep(2 * time.Second)
	}
}

// addressOf is the guest's allocated address, which is what a client in the
// VPC connects to.
func (h *node) addressOf(t *testing.T, instanceID string) string {
	t.Helper()

	rows, err := h.store.ListInterfacesByInstances(t.Context(),
		sqlc.ListInterfacesByInstancesParams{InstanceIds: []string{instanceID}})
	if err != nil {
		t.Fatalf("ListInterfacesByInstances(): %v", err)
	}
	if len(rows) == 0 {
		t.Fatalf("the server %s has no interface", instanceID)
	}
	return rows[0].PrivateIp
}

// resolveInVPC asks the VPC's own resolver for a name, which is the dnsmasq
// libvirt runs on the bridge. The host does not use it, so the query is sent
// to the gateway directly.
func (h *node) resolveInVPC(t *testing.T, names []string) string {
	t.Helper()

	vpcRow, err := h.vpcs.Get(t.Context(), h.vpcID)
	if err != nil {
		t.Fatalf("Get(VPC): %v", err)
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := net.Dialer{Timeout: 5 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(vpcRow.GatewayIP, "53"))
		},
	}

	var problems []string
	for _, name := range names {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		addrs, err := resolver.LookupHost(ctx, name)
		cancel()
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if len(addrs) > 0 {
			t.Logf("%s resolves to %v in the VPC", name, addrs)
			return addrs[0]
		}
	}
	t.Fatalf("no name resolved in the VPC: %s", strings.Join(problems, "; "))
	return ""
}

// hostAddressOutside is a host address that the DB instance's VPC does not
// hold, for the client-from-outside check. It returns nil when the host has
// none, which is a node whose only addresses are the VPC's own.
func (h *node) hostAddressOutside(t *testing.T) net.Addr {
	t.Helper()

	prefix, err := netip.ParsePrefix(h.vpcCIDR)
	if err != nil {
		t.Fatalf("ParsePrefix(%q): %v", h.vpcCIDR, err)
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("InterfaceAddrs(): %v", err)
	}
	for _, candidate := range addrs {
		ipNet, ok := candidate.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipNet.IP.To4())
		if !ok || !addr.Is4() || addr.IsLoopback() || prefix.Contains(addr) {
			continue
		}
		return &net.TCPAddr{IP: ipNet.IP.To4()}
	}
	return nil
}

// assertDisks reads the domain back from libvirt and checks the shape §4 step 5
// prescribes: the root overlay, the seed and the data disk, the last one
// carrying the serial the guest finds it by.
func (h *node) assertDisks(t *testing.T, db dbinstance.DBInstance) {
	t.Helper()

	xml := domainXML(t, db.InstanceID)
	for _, want := range []string{
		hypervisor.RootDiskName(db.InstanceID),
		hypervisor.SeedISOName(db.InstanceID),
		hypervisor.DataDiskName(db.ID),
		"<serial>" + hypervisor.DataDiskSerial + "</serial>",
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("the domain XML does not carry %q", want)
		}
	}
}

// domainXML reads a domain definition back from libvirtd.
//
// It goes through go-libvirt directly rather than through the hypervisor
// interface: reading a definition is something a test wants and nothing in
// Kumo needs, so the interface has no method for it and should not grow one
// for this.
func domainXML(t *testing.T, name string) string {
	t.Helper()

	socket := os.Getenv("KUMO_TEST_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/var/run/libvirt/libvirt-sock"
	}
	conn := golibvirt.NewWithDialer(dialers.NewLocal(dialers.WithSocket(socket)))
	if err := conn.ConnectToURI(golibvirt.ConnectURI("qemu:///system")); err != nil {
		t.Fatalf("connect to libvirt: %v", err)
	}
	defer func() { _ = conn.Disconnect() }()

	domain, err := conn.DomainLookupByName(name)
	if err != nil {
		t.Fatalf("DomainLookupByName(%q): %v", name, err)
	}
	xml, err := conn.DomainGetXMLDesc(domain, 0)
	if err != nil {
		t.Fatalf("DomainGetXMLDesc(%q): %v", name, err)
	}
	return xml
}

// assertTaskPayloadsClean proves the rows anybody with the database can read
// carry no credential: the create task names the DB instance and nothing else
// (07-databases.md §5).
func (h *node) assertTaskPayloadsClean(t *testing.T, dbInstanceID, password string) {
	t.Helper()

	rows, err := h.store.DB().QueryContext(t.Context(),
		`SELECT type, payload FROM tasks WHERE resource_id = ?`, dbInstanceID)
	if err != nil {
		t.Fatalf("read the tasks: %v", err)
	}
	defer rows.Close() //nolint:errcheck // a read-only cursor

	seen := 0
	for rows.Next() {
		var taskType, payload string
		if err := rows.Scan(&taskType, &payload); err != nil {
			t.Fatalf("scan a task: %v", err)
		}
		seen++
		if strings.Contains(payload, password) || strings.Contains(payload, "SCRAM-SHA-256$") {
			t.Errorf("the %s payload carries a credential", taskType)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Errorf("the %s payload is not an object: %v", taskType, err)
			continue
		}
		if len(decoded) != 1 || decoded["db_instance_id"] != dbInstanceID {
			t.Errorf("the %s payload is %v, want only the DB instance id", taskType, decoded)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the tasks: %v", err)
	}
	if seen == 0 {
		t.Error("no task rows for the DB instance")
	}
}

// deleteDatabase deletes a DB instance and waits for the task, tolerating one
// that is already gone: it is both a step of the test and its cleanup.
func (h *node) deleteDatabase(t *testing.T, dbInstanceID string) {
	t.Helper()

	ctx := context.WithoutCancel(t.Context())
	if _, err := h.databases.Get(ctx, dbInstanceID); err != nil {
		return
	}
	if _, err := h.databases.Delete(ctx, dbInstanceID); err != nil {
		t.Logf("delete the DB instance: %v", err)
	}

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := h.databases.Get(ctx, dbInstanceID); err != nil {
			return
		}
		time.Sleep(time.Second)
	}
	t.Errorf("the DB instance was still there three minutes after the delete\nservice log:\n%s",
		h.logTail(40))
}

// logs is everything the services have written so far.
func (h *node) logs() string { return h.logBuffer.String() }

// logTail is the last n log lines, which is what a failure wants to show: the
// test is the only place this log exists, so a failure that did not print it
// would leave nothing to debug from.
func (h *node) logTail(n int) string {
	lines := strings.Split(strings.TrimSpace(h.logs()), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// reasonOf renders a DB instance's state reason, which is a pointer because
// the column is nullable.
func reasonOf(db dbinstance.DBInstance) string {
	if db.StateReason == nil {
		return "(no reason recorded)"
	}
	return *db.StateReason
}

// syncBuffer is a slog destination a test can read while the workers write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
