//go:build libvirt

package kubernetes_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	kumolibvirt "github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/kubernetes"
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

// busybox is the one workload image the Kubernetes engine image carries
// besides the cluster's own (packaging/images/kubernetes-1.37): the nodes have
// no registry, so a workload must come from the image's store.
const busybox = "docker.io/library/busybox:1.38.0@sha256:a46257e06c82ad85242d0d38077d0a52d9896389ca60a1a59d3463db586e5fa4"

// TestClusterJourney is the M8.8 acceptance run on real hardware: a cluster
// created through the real services on a real libvirtd, from the M8.1 image,
// and used the way a user uses one (docs/design/08-kubernetes.md).
//
// What only a real node can prove:
//
//   - the create task brings up a control plane and workers that join it, and
//     the health pass sees every node Ready;
//   - the endpoint resolves in the VPC's DNS to the control plane;
//   - a downloaded kubeconfig deploys a workload and a NetworkPolicy, and
//     Cilium enforces the policy; a viewer's kubeconfig is refused a write;
//   - a scale up joins a real node and a scale down drains and removes real
//     ones;
//   - a delete takes every VM, every volume, the DNS name and the PKI
//     directory with it;
//   - no key and no token reaches kumod's log.
//
// The client stands where an instance of the VPC stands: on the VPC's bridge,
// resolving the endpoint through the VPC's own resolver. The host is on that
// bridge as its gateway, which is the address every guest of the VPC routes
// through, so nothing it reaches is out of a guest's reach.
//
//	KUMO_TEST_K8S_IMAGE=/path/to/kumo-kubernetes-1.37-<date>.qcow2[.gz] make test-k8s-journey
//
// It takes about 15 minutes and 14 GiB of memory at its peak (four nodes).
func TestClusterJourney(t *testing.T) {
	imagePath := os.Getenv("KUMO_TEST_K8S_IMAGE")
	if imagePath == "" {
		t.Skip("set KUMO_TEST_K8S_IMAGE to the image from packaging/images/kubernetes-1.37 to run this")
	}
	if _, err := os.Stat(imagePath); err != nil {
		t.Fatalf("KUMO_TEST_K8S_IMAGE: %v", err)
	}

	h := newClusterNode(t)
	h.importImage(t, imagePath)

	created, err := h.clusters.Create(t.Context(), kubernetes.CreateInput{
		Name:             "it-shop",
		ControlPlaneType: "kumo.medium",
		NodeType:         "kumo.medium",
		NodeCount:        2,
	})
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	t.Logf("creating %s", created.ID)
	t.Cleanup(func() { h.deleteCluster(t, created.ID) })

	start := time.Now()
	ready := h.awaitCluster(t, created.ID, 25*time.Minute, func(c kubernetes.Cluster) bool {
		return c.State == kubernetes.StateActive && allReady(c) && len(c.Nodes) == 3
	})
	t.Logf("active with every node Ready after %s", time.Since(start).Round(time.Second))
	if ready.Endpoint == nil {
		t.Fatal("the cluster is active with no endpoint")
	}
	controlPlaneIP := ""
	for _, n := range ready.Nodes {
		if n.Role == kubernetes.RoleControlPlane {
			controlPlaneIP = n.PrivateIP
		}
	}
	endpointHost := strings.TrimSuffix(strings.TrimPrefix(*ready.Endpoint, "https://"), ":6443")

	t.Run("the endpoint resolves in the VPC to the control plane", func(t *testing.T) {
		if want := "api.it-shop." + h.vpcName; endpointHost != want {
			t.Errorf("endpoint host = %q, want %q", endpointHost, want)
		}
		if got := h.resolveInVPC(t, endpointHost); got != controlPlaneIP {
			t.Errorf("%s resolves to %q, want the control plane's %q", endpointHost, got, controlPlaneIP)
		}
	})

	adminFile := h.mint(t, created.ID, "ada", auth.RoleAdmin)
	viewerFile := h.mint(t, created.ID, "vic", auth.RoleViewer)
	admin := h.kubeClient(t, adminFile)
	viewer := h.kubeClient(t, viewerFile)
	base := *ready.Endpoint

	t.Run("a viewer kubeconfig reads and is refused a write", func(t *testing.T) {
		// `view` is Kubernetes' own role: namespaced reads, so pods yes and
		// nodes (cluster-scoped) no.
		expectStatus(t, viewer, http.MethodGet, base+"/api/v1/namespaces/default/pods", "", http.StatusOK)
		expectStatus(t, viewer, http.MethodPost, base+"/api/v1/namespaces/default/configmaps",
			`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"by-viewer"}}`, http.StatusForbidden)
	})

	t.Run("a workload and a NetworkPolicy, enforced", func(t *testing.T) {
		h.deployWeb(t, admin, base)

		// Open: with no policy, any pod reaches the service by its name,
		// through CoreDNS and the service's virtual address.
		if phase := runProbe(t, admin, base, "probe-open", nil); phase != "Succeeded" {
			t.Fatalf("a probe with no policy in place ended %s, want Succeeded", phase)
		}

		// The policy admits only pods labeled access=granted.
		expectStatus(t, admin, http.MethodPost, base+"/apis/networking.k8s.io/v1/namespaces/default/networkpolicies", `{
			"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"web-allow"},
			"spec":{"podSelector":{"matchLabels":{"app":"web"}},"policyTypes":["Ingress"],
			"ingress":[{"from":[{"podSelector":{"matchLabels":{"access":"granted"}}}],
			"ports":[{"protocol":"TCP","port":8080}]}]}}`, http.StatusCreated)
		// Cilium computes and pushes the policy to every node's agent; give
		// it a moment rather than racing it.
		time.Sleep(15 * time.Second)

		if phase := runProbe(t, admin, base, "probe-denied", nil); phase != "Failed" {
			t.Errorf("an unlabelled probe under the policy ended %s, want Failed", phase)
		}
		if phase := runProbe(t, admin, base, "probe-granted", map[string]string{"access": "granted"}); phase != "Succeeded" {
			t.Errorf("a granted probe under the policy ended %s, want Succeeded", phase)
		}
	})

	t.Run("scale 2 to 3 joins a node", func(t *testing.T) {
		h.scaleTo(t, ready, 3)
		got := h.awaitCluster(t, created.ID, 15*time.Minute, func(c kubernetes.Cluster) bool {
			return c.State == kubernetes.StateActive && len(c.Nodes) == 4 && allReady(c)
		})
		if names := strings.Join(workerNames(got), " "); names != "it-shop-default-1 it-shop-default-2 it-shop-default-3" {
			t.Errorf("workers = %s", names)
		}
		if n := readyNodes(t, admin, base); n != 4 {
			t.Errorf("the API server reports %d Ready nodes, want 4", n)
		}
	})

	t.Run("scale 3 to 1 drains and removes the newest", func(t *testing.T) {
		before := h.get(t, created.ID)
		gone := map[string]string{}
		for _, n := range before.Nodes {
			if n.Name == "it-shop-default-2" || n.Name == "it-shop-default-3" {
				gone[n.Name] = n.InstanceID
			}
		}
		h.scaleTo(t, before, 1)
		got := h.awaitCluster(t, created.ID, 15*time.Minute, func(c kubernetes.Cluster) bool {
			return c.State == kubernetes.StateActive && len(c.Nodes) == 2
		})
		if names := strings.Join(workerNames(got), " "); names != "it-shop-default-1" {
			t.Errorf("workers = %s, want it-shop-default-1", names)
		}
		if n := readyNodes(t, admin, base); n != 2 {
			t.Errorf("the API server reports %d Ready nodes, want 2", n)
		}
		for name, id := range gone {
			h.assertVMGone(t, name, id)
		}
		// The workload survived the drain: its pods were evicted onto the
		// node that is left.
		h.awaitAvailable(t, admin, base, 2)
	})

	t.Run("no key or token is in the log", func(t *testing.T) {
		logged := h.logs()
		if strings.Contains(logged, "PRIVATE KEY") {
			t.Error("the log carries a private key")
		}
		for _, file := range [][]byte{adminFile, viewerFile} {
			if key := clientKeyData(t, file); strings.Contains(logged, key) {
				t.Error("the log carries a kubeconfig's client key")
			}
		}
		// A bootstrap token is [a-z0-9]{6}.[a-z0-9]{16} (K8s's own format).
		if m := regexp.MustCompile(`\b[a-z0-9]{6}\.[a-z0-9]{16}\b`).FindString(logged); m != "" {
			t.Errorf("the log carries something shaped like a bootstrap token: %q", m)
		}
	})

	t.Run("a delete takes every VM, volume, name and the PKI", func(t *testing.T) {
		last := h.get(t, created.ID)
		h.deleteCluster(t, created.ID)
		for _, n := range last.Nodes {
			h.assertVMGone(t, n.Name, n.InstanceID)
		}
		if _, err := os.Stat(filepath.Join(h.dataDir, "clusters", created.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the PKI directory survived the delete (stat: %v)", err)
		}
		if addr, err := h.lookupInVPC(endpointHost); err == nil {
			t.Errorf("%s still resolves to %v after the delete", endpointHost, addr)
		}
	})
}

// clusterNode is the control plane of the journey: the real services over a
// real libvirtd, wired the way internal/cli/serve.go wires them.
type clusterNode struct {
	store         *store.Store
	hv            *kumolibvirt.Client
	images        *image.Service
	clusters      *kubernetes.Service
	vpcs          *vpc.Service
	instancesPool string
	vpcID         string
	vpcName       string
	gateway       string
	dataDir       string
	logBuffer     *lockedBuffer
}

func newClusterNode(t *testing.T) *clusterNode {
	t.Helper()

	socket := os.Getenv("KUMO_TEST_LIBVIRT_SOCKET")
	if socket == "" {
		socket = "/var/run/libvirt/libvirt-sock"
	}
	logBuffer := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logBuffer, &slog.HandlerOptions{Level: slog.LevelDebug}))

	hv, err := kumolibvirt.New(logger, "unix://"+socket)
	if err != nil {
		t.Skipf("no libvirt at %s: %v", socket, err)
	}
	if err := hv.Ping(t.Context()); err != nil {
		t.Skipf("libvirt at %s does not answer: %v", socket, err)
	}
	t.Cleanup(func() { _ = hv.Close() })

	// The pools hold a 4 GiB image and four root disks: not a tmpfs.
	dataDir := os.Getenv("KUMO_TEST_DATA_DIR")
	if dataDir == "" {
		dataDir = t.TempDir()
	} else {
		dataDir = filepath.Join(dataDir, "kumo-it-k8s")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", dataDir, err)
		}
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("Chmod(%s): %v", dataDir, err)
	}
	consoleDir := filepath.Join(dataDir, "console")
	if err := os.MkdirAll(consoleDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", consoleDir, err)
	}
	labelForVirtlogd(t, consoleDir)

	const (
		imagesPool    = "kumo-it-k8s-images"
		instancesPool = "kumo-it-k8s-instances"
		network       = "kumo-it-k8s-net"
	)
	if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: network, DataDir: dataDir,
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}

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
	vpcCIDR := os.Getenv("KUMO_TEST_VPC_CIDR")
	if vpcCIDR == "" {
		vpcCIDR = "10.178.0.0/24"
	}
	vpcs, err := vpc.New(vpc.Options{
		Logger: logger, Store: db, Hypervisor: hv, Tasks: tasks, Subnets: subnets, DefaultVPCCIDR: vpcCIDR,
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
		Logger: logger, Store: db, Hypervisor: hv, Tasks: tasks,
		Downloader: imagefetch.New(imagefetch.Options{MaxBytes: 16 << 30, AllowPrivateNetworks: true}),
		ImagesPool: imagesPool, TempDir: staging, MaxBytes: 16 << 30,
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}

	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys := system.New(logger, "test", "libvirt", db, hv, db)
	sys.SetLimits(limits)

	defaultVPC, err := vpcs.Bootstrap(t.Context(), network)
	if err != nil {
		t.Fatalf("vpc.Bootstrap(): %v", err)
	}
	t.Logf("VPC %s (%s) on %s", defaultVPC.ID, defaultVPC.Name, defaultVPC.CIDR)

	instances, err := instance.New(instance.Options{
		Logger: logger, Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks,
		Images: images, Types: types, KeyPairs: pairs, Capacity: sys,
		InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: consoleDir,
		Network:    defaultVPC.LibvirtNetwork, SubnetLocks: subnetLocks, Limits: limits,
		PollInterval: 2 * time.Second, PollTimeout: 3 * time.Minute, StopTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}
	clusters, err := kubernetes.New(kubernetes.Options{
		Logger: logger, Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
		DataDir: dataDir, API: kubernetes.RealAPI,
		ReadyTimeout: 20 * time.Minute, ProbeInterval: 10 * time.Second, PollInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("kubernetes.New(): %v", err)
	}

	vpcRow, err := vpcs.Get(t.Context(), defaultVPC.ID)
	if err != nil {
		t.Fatalf("Get(VPC): %v", err)
	}
	out := &clusterNode{
		store: db, hv: hv, images: images, clusters: clusters, vpcs: vpcs,
		instancesPool: instancesPool, vpcID: defaultVPC.ID, vpcName: defaultVPC.Name,
		gateway: vpcRow.GatewayIP, dataDir: dataDir, logBuffer: logBuffer,
	}

	// The workers and the reconciler run under a context of their own: Go
	// cancels t.Context() before the cleanups, which queue deletes.
	runCtx, stopRun := context.WithCancel(context.Background())
	registry := worker.Registry{}
	for _, r := range []worker.Registry{images.Handler(), instances.Handler(), vpcs.Handler(), clusters.Handler()} {
		for k, v := range r {
			registry[k] = v
		}
	}
	pool := worker.New(worker.Options{
		Logger: logger, Store: db, Workers: 3, PollInterval: 200 * time.Millisecond, Registry: registry,
	})
	if err := pool.Start(runCtx); err != nil {
		stopRun()
		t.Fatalf("worker.Start(): %v", err)
	}
	reconciler, err := worker.NewReconciler(worker.ReconcilerOptions{
		Logger: logger, Interval: 5 * time.Second,
		Pass: func(ctx context.Context) error {
			if _, err := instances.Reconcile(ctx); err != nil {
				return err
			}
			_, err := clusters.Reconcile(ctx)
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

	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if row, err := db.GetDefaultSubnet(ctx); err == nil {
			if err := subnets.Delete(ctx, row.ID); err != nil {
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
		_ = reconciler.Stop(stopCtx)
		_ = pool.Stop(stopCtx)
		stopRun()
	})
	return out
}

// labelForVirtlogd gives the console directory the SELinux type virtlogd may
// write, on a host that enforces SELinux. A packaged node gets it from
// scripts/security-config.sh; a test's directory under /var/tmp is user_tmp_t,
// and every guest would fail to start with "Unable to open file ... Permission
// denied" on its console log. The directory is the test's own, so no
// privilege is needed to relabel it.
func labelForVirtlogd(t *testing.T, dir string) {
	t.Helper()
	enforce, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil || strings.TrimSpace(string(enforce)) != "1" {
		return
	}
	if out, err := exec.CommandContext(t.Context(), "chcon", "-t", "virt_log_t", dir).CombinedOutput(); err != nil {
		t.Fatalf("SELinux is enforcing and %s cannot be labeled virt_log_t: %v: %s", dir, err, out)
	}
}

// importImage serves the image over loopback and imports it as an operator
// would: a URL and the engine fields. A .gz goes through the gzip transport.
func (h *clusterNode) importImage(t *testing.T, path string) {
	t.Helper()

	server := serveFile(t, path)
	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name:          "kumo-it-kubernetes-1.37",
		SourceURL:     server + "/" + filepath.Base(path),
		OSFamily:      "linux",
		Architecture:  "x86_64",
		DefaultUser:   "ubuntu",
		MinDiskGB:     8,
		Kind:          image.KindEngine,
		Engine:        image.EngineKubernetes,
		EngineVersion: "1.37",
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(t.Context())
		if _, err := h.images.Delete(ctx, imported.ID); err != nil {
			t.Logf("delete the image: %v", err)
			return
		}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if _, err := h.images.Get(ctx, imported.ID); err != nil {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	})

	deadline := time.Now().Add(15 * time.Minute)
	for {
		got, err := h.images.Get(t.Context(), imported.ID)
		if err != nil {
			t.Fatalf("Get(%q): %v", imported.ID, err)
		}
		switch got.State {
		case image.StateAvailable:
			t.Logf("image %s available", got.ID)
			return
		case image.StateFailed:
			t.Fatalf("the import failed: %v", got.StateReason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the image is %s after 15 minutes", got.State)
		}
		time.Sleep(time.Second)
	}
}

func serveFile(t *testing.T, path string) string {
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

func (h *clusterNode) get(t *testing.T, id string) kubernetes.Cluster {
	t.Helper()
	c, err := h.clusters.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return c
}

// awaitCluster waits for a cluster to satisfy done, failing early on failed.
func (h *clusterNode) awaitCluster(
	t *testing.T, id string, timeout time.Duration, done func(kubernetes.Cluster) bool,
) kubernetes.Cluster {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c := h.get(t, id)
		if done(c) {
			return c
		}
		if c.State == kubernetes.StateFailed {
			t.Logf("service log:\n%s", h.logTail(60))
			t.Fatalf("the cluster failed: %s", reason(c))
		}
		if time.Now().After(deadline) {
			t.Logf("service log:\n%s", h.logTail(60))
			t.Fatalf("after %s the cluster is %s (%s) with nodes %+v", timeout, c.State, reason(c), c.Nodes)
		}
		time.Sleep(3 * time.Second)
	}
}

func (h *clusterNode) scaleTo(t *testing.T, c kubernetes.Cluster, desired int) {
	t.Helper()
	if _, err := h.clusters.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, desired); err != nil {
		t.Fatalf("Scale(%d): %v", desired, err)
	}
	// The scale is queued; wait for the task to take the cluster through
	// updating, so the await below does not read the state before it.
	h.awaitCluster(t, c.ID, time.Minute, func(c kubernetes.Cluster) bool {
		return c.NodeGroups[0].DesiredSize == desired
	})
}

// mint is a kubeconfig as the console's download hands it over.
func (h *clusterNode) mint(t *testing.T, id, user string, role auth.Role) []byte {
	t.Helper()
	k, err := h.clusters.MintKubeconfig(t.Context(), id, user, role)
	if err != nil {
		t.Fatalf("MintKubeconfig(%s): %v", role, err)
	}
	return k.Kubeconfig.Bytes()
}

// kubeClient builds an HTTP client from a kubeconfig file alone, dialing the
// way a VPC instance does: the endpoint's name through the VPC's resolver.
func (h *clusterNode) kubeClient(t *testing.T, file []byte) *http.Client {
	t.Helper()
	var doc struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
				CA     string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User struct {
				Cert string `yaml:"client-certificate-data"`
				Key  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(file, &doc); err != nil || len(doc.Clusters) != 1 || len(doc.Users) != 1 {
		t.Fatalf("parse the kubeconfig: %v", err)
	}
	ca, _ := base64.StdEncoding.DecodeString(doc.Clusters[0].Cluster.CA)
	cert, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Cert)
	key, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Key)
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatalf("the kubeconfig's client certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("the kubeconfig's CA does not parse")
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12},
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				addrs, err := h.vpcResolver().LookupHost(ctx, host)
				if err != nil {
					return nil, err
				}
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort(addrs[0], port))
			},
		},
	}
}

func (h *clusterNode) vpcResolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, net.JoinHostPort(h.gateway, "53"))
		},
	}
}

func (h *clusterNode) lookupInVPC(name string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return h.vpcResolver().LookupHost(ctx, name)
}

func (h *clusterNode) resolveInVPC(t *testing.T, name string) string {
	t.Helper()
	addrs, err := h.lookupInVPC(name)
	if err != nil || len(addrs) == 0 {
		t.Fatalf("%s does not resolve in the VPC: %v", name, err)
	}
	return addrs[0]
}

// deployWeb is two busybox web servers behind a service, waited on.
func (h *clusterNode) deployWeb(t *testing.T, c *http.Client, base string) {
	t.Helper()
	expectStatus(t, c, http.MethodPost, base+"/apis/apps/v1/namespaces/default/deployments", `{
		"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web"},
		"spec":{"replicas":2,"selector":{"matchLabels":{"app":"web"}},
		"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"httpd",
		"image":"`+busybox+`","imagePullPolicy":"IfNotPresent",
		"command":["sh","-c","echo ok > /tmp/index.html && exec httpd -f -p 8080 -h /tmp"],
		"ports":[{"containerPort":8080}]}]}}}}`, http.StatusCreated)
	expectStatus(t, c, http.MethodPost, base+"/api/v1/namespaces/default/services", `{
		"apiVersion":"v1","kind":"Service","metadata":{"name":"web"},
		"spec":{"selector":{"app":"web"},"ports":[{"port":80,"targetPort":8080}]}}`, http.StatusCreated)
	h.awaitAvailable(t, c, base, 2)
}

func (h *clusterNode) awaitAvailable(t *testing.T, c *http.Client, base string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	pattern := fmt.Sprintf(`"availableReplicas":%d`, want)
	for time.Now().Before(deadline) {
		status, body := do(c, http.MethodGet, base+"/apis/apps/v1/namespaces/default/deployments/web", "")
		if status == http.StatusOK && strings.Contains(body, pattern) {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("the web deployment never had %d available replicas", want)
}

// runProbe runs a pod that fetches the web service once and returns the phase
// it ends in: Succeeded when it got an answer, Failed when it did not.
func runProbe(t *testing.T, c *http.Client, base, name string, labels map[string]string) string {
	t.Helper()
	var labelJSON strings.Builder
	labelJSON.WriteString(`{"probe":"web"`)
	for k, v := range labels {
		fmt.Fprintf(&labelJSON, `,%q:%q`, k, v)
	}
	labelJSON.WriteString("}")
	expectStatus(t, c, http.MethodPost, base+"/api/v1/namespaces/default/pods", `{
		"apiVersion":"v1","kind":"Pod","metadata":{"name":"`+name+`","labels":`+labelJSON.String()+`},
		"spec":{"restartPolicy":"Never","containers":[{"name":"wget","image":"`+busybox+`",
		"imagePullPolicy":"IfNotPresent","command":["wget","-q","-T","5","-O-","http://web/"]}]}}`, http.StatusCreated)
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		status, body := do(c, http.MethodGet, base+"/api/v1/namespaces/default/pods/"+name, "")
		if status == http.StatusOK {
			for _, phase := range []string{"Succeeded", "Failed"} {
				if strings.Contains(body, `"phase":"`+phase+`"`) {
					t.Logf("%s ended %s", name, phase)
					return phase
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("%s never finished", name)
	return ""
}

func readyNodes(t *testing.T, c *http.Client, base string) int {
	t.Helper()
	status, body := do(c, http.MethodGet, base+"/api/v1/nodes", "")
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/nodes = %d: %s", status, body)
	}
	return strings.Count(body, `"type":"Ready","status":"True"`)
}

func (h *clusterNode) assertVMGone(t *testing.T, name, instanceID string) {
	t.Helper()
	if _, err := h.hv.GetDomain(t.Context(), instanceID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("%s's domain survived (err = %v)", name, err)
	}
	if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: instanceID}); err == nil {
		t.Errorf("%s's instance row survived", name)
	}
	for _, volume := range []string{hypervisor.RootDiskName(instanceID), hypervisor.SeedISOName(instanceID)} {
		if _, err := h.hv.VolumePath(t.Context(), h.instancesPool, volume); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("%s's volume %s survived (err = %v)", name, volume, err)
		}
	}
}

// deleteCluster deletes a cluster and waits for it to go, tolerating one that
// is already gone: it is a step of the test and its cleanup.
func (h *clusterNode) deleteCluster(t *testing.T, id string) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	c, err := h.clusters.Get(ctx, id)
	if err != nil {
		return
	}
	if c.State != kubernetes.StateDeleting {
		if _, err := h.clusters.Delete(ctx, id); err != nil {
			t.Logf("delete the cluster: %v", err)
		}
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := h.clusters.Get(ctx, id); err != nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Errorf("the cluster was still there five minutes after the delete\nservice log:\n%s", h.logTail(40))
}

func (h *clusterNode) logs() string { return h.logBuffer.String() }

func (h *clusterNode) logTail(n int) string {
	lines := strings.Split(strings.TrimSpace(h.logs()), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func allReady(c kubernetes.Cluster) bool {
	for _, n := range c.Nodes {
		if n.Ready == nil || !*n.Ready {
			return false
		}
	}
	return len(c.Nodes) > 0
}

func reason(c kubernetes.Cluster) string {
	if c.StateReason == nil {
		return "no reason recorded"
	}
	return *c.StateReason
}

func clientKeyData(t *testing.T, file []byte) string {
	t.Helper()
	var doc struct {
		Users []struct {
			User struct {
				Key string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(file, &doc); err != nil || len(doc.Users) != 1 || doc.Users[0].User.Key == "" {
		t.Fatalf("the kubeconfig has no client key: %v", err)
	}
	return doc.Users[0].User.Key
}

func do(c *http.Client, method, url, body string) (int, string) {
	req, err := http.NewRequest(method, url, strings.NewReader(body)) //nolint:noctx // a test's bounded client
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close() //nolint:errcheck // a read-only body
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func expectStatus(t *testing.T, c *http.Client, method, url, body string, want int) {
	t.Helper()
	if status, got := do(c, method, url, body); status != want {
		t.Errorf("%s %s = %d, want %d: %s", method, url, status, want, got)
	}
}

// lockedBuffer is a slog destination the test reads while the workers write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
