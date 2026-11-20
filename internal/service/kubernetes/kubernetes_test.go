package kubernetes_test

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/kubernetes"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"
	imageDiskGB   = 1
	nodeType      = "kumo.medium"
)

// cluster is a scriptable stand-in for a cluster's API server.
type cluster struct {
	mu       sync.Mutex
	ready    bool
	notReady map[string]bool
	tokens   []k8spki.BootstrapToken
	conns    []kubernetes.Connection
	// pods are the pods on each node, for a drain; blocked are the ones a
	// disruption budget protects; calls is every drain call, in order.
	pods    map[string][]k8sapi.Pod
	blocked map[string]bool
	calls   []string
}

func (c *cluster) record(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
}

func (c *cluster) factory(conn kubernetes.Connection) (kubernetes.ClusterAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conns = append(c.conns, conn)
	return &scripted{c: c, nodes: conn.Nodes}, nil
}

func (c *cluster) set(ready bool, notReady ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = ready
	c.notReady = map[string]bool{}
	for _, n := range notReady {
		c.notReady[n] = true
	}
}

type scripted struct {
	c     *cluster
	nodes []string
}

var errNotReady = errors.New("not ready")

func (s *scripted) Ready(context.Context) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if !s.c.ready {
		return errNotReady
	}
	return nil
}

func (s *scripted) Nodes(context.Context) ([]k8sapi.Node, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if !s.c.ready {
		return nil, errNotReady
	}
	var out []k8sapi.Node
	for _, n := range s.nodes {
		out = append(out, k8sapi.Node{Name: n, Ready: !s.c.notReady[n]})
	}
	return out, nil
}

func (s *scripted) CreateBootstrapToken(_ context.Context, tok k8spki.BootstrapToken, _ time.Time) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	s.c.tokens = append(s.c.tokens, tok)
	return nil
}
func (s *scripted) Cordon(_ context.Context, node string) error {
	s.c.record("cordon " + node)
	return nil
}

func (s *scripted) PodsOnNode(_ context.Context, node string) ([]k8sapi.Pod, error) {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return append([]k8sapi.Pod(nil), s.c.pods[node]...), nil
}

func (s *scripted) Evict(_ context.Context, ns, pod string) error {
	s.c.mu.Lock()
	blocked := s.c.blocked[ns+"/"+pod]
	s.c.mu.Unlock()
	if blocked {
		s.c.record("refused " + ns + "/" + pod)
		return fmt.Errorf("%w: %s/%s", k8sapi.ErrEvictionBlocked, ns, pod)
	}
	s.c.record("evict " + ns + "/" + pod)
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	for node, pods := range s.c.pods {
		kept := pods[:0]
		for _, p := range pods {
			if p.Namespace != ns || p.Name != pod {
				kept = append(kept, p)
			}
		}
		s.c.pods[node] = kept
	}
	return nil
}

func (s *scripted) DeleteNode(_ context.Context, node string) error {
	s.c.record("delete " + node)
	return nil
}
func (s *scripted) Close() {}

type harness struct {
	svc       *kubernetes.Service
	instances *instance.Service
	tasks     *task.Service
	store     *store.Store
	hv        *fake.Fake
	images    *image.Service
	api       *cluster
	dataDir   string
	vpcID     string
	now       *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()
	ctx := t.Context()
	db := storetest.New(t)
	// 32 GiB: room for a few small clusters of 4 GiB nodes, and still a
	// ceiling an eleven-node cluster goes through.
	hv := fake.New(fake.Config{Capacity: hypervisor.Capacity{VCPUs: 16, MemoryBytes: 32 << 30, DiskBytes: 500 << 30}})
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: "kumo-default", DataDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	vpcID := seedNetwork(t, db, hv)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	types, _ := instancetype.New(instancetype.Options{Logger: logging.Discard(), Store: db})
	tasks, _ := task.New(task.Options{Store: db, Now: clock})
	images, err := image.New(image.Options{
		Logger: logging.Discard(), Store: db, Hypervisor: hv, Tasks: tasks,
		Downloader: imagefetch.New(imagefetch.Options{MaxBytes: 1 << 20}),
		ImagesPool: imagesPool, TempDir: t.TempDir(), MaxBytes: 4 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	pairs, _ := keypair.New(keypair.Options{Logger: logging.Discard(), Store: db})
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys := system.New(logging.Discard(), "test", "fake", db, hv, db)
	sys.SetLimits(limits)
	instances, err := instance.New(instance.Options{
		Logger: logging.Discard(), Store: db, Hypervisor: hv, Volumes: hv, Tasks: tasks, Images: images,
		Types: types, KeyPairs: pairs, Capacity: sys, InstancesPool: instancesPool, ImagesPool: imagesPool,
		ConsoleDir: t.TempDir(), Network: vpcID, Limits: limits,
		PollInterval: time.Millisecond, PollTimeout: time.Second, StopTimeout: 100 * time.Millisecond, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	api := &cluster{ready: true, notReady: map[string]bool{}, pods: map[string][]k8sapi.Pod{}, blocked: map[string]bool{}}
	dataDir := t.TempDir()
	svc, err := kubernetes.New(kubernetes.Options{
		Logger: logging.Discard(), Store: db, Hypervisor: hv, Instances: instances, Tasks: tasks,
		DataDir: dataDir, API: api.factory,
		ReadyTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond, ProbeInterval: time.Minute,
		DrainTimeout: 50 * time.Millisecond,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := harness{svc: svc, instances: instances, tasks: tasks, store: db, hv: hv, images: images, api: api,
		dataDir: dataDir, vpcID: vpcID, now: &now}
	return h
}

func seedNetwork(t *testing.T, db *store.Store, hv *fake.Fake) string {
	t.Helper()
	vpcID, subnetID := id.VPC(), id.Subnet()
	if _, err := db.Exec(t.Context(),
		`INSERT INTO vpcs (id, name, cidr, mode, gateway_ip, netmask, bridge_name,
		   libvirt_network, is_default, created_at, updated_at)
		 VALUES (?1, 'default', '192.168.100.0/24', 'nat', '192.168.100.1', '255.255.255.0', ?2, ?1, 1,
		        '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`, vpcID, hypervisor.BridgeName(vpcID)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(),
		`INSERT INTO subnets (id, vpc_id, name, cidr, is_default, created_at, updated_at)
		 VALUES (?1, ?2, 'default', '192.168.100.0/25', 1, '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		subnetID, vpcID); err != nil {
		t.Fatal(err)
	}
	if err := hv.DefineNetwork(t.Context(), hypervisor.NetworkSpec{
		Name: vpcID, Bridge: hypervisor.BridgeName(vpcID), CIDR: "192.168.100.0/24",
		Gateway: "192.168.100.1", Mode: hypervisor.NetworkModeNAT, Domain: "default",
	}); err != nil {
		t.Fatal(err)
	}
	if err := hv.StartNetwork(t.Context(), vpcID); err != nil {
		t.Fatal(err)
	}
	return vpcID
}

// kubernetesImage publishes an available kubernetes engine image the way the
// import workflow would.
func (h *harness) kubernetesImage(t *testing.T, version string) {
	t.Helper()
	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name: "kubernetes-" + version, SourceURL: "https://example.invalid/k8s.qcow2.gz", DefaultUser: "ubuntu",
		MinDiskGB: imageDiskGB, Kind: image.KindEngine, Engine: image.EngineKubernetes, EngineVersion: version,
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	body := qcow2test.Tiny(imageDiskGB << 30)
	path := filepath.Join(t.TempDir(), "k8s.qcow2")
	_ = os.WriteFile(path, body, 0o600)
	file, _ := os.Open(path)
	defer func() { _ = file.Close() }()
	if err := h.hv.ImportImage(t.Context(), imported.ID, file, hypervisor.ImportSpec{
		SizeBytes: int64(len(body)), VirtualSizeBytes: imageDiskGB << 30, Format: hypervisor.FormatQCOW2,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.FinalizeImage(t.Context(), sqlc.FinalizeImageParams{
		ID: imported.ID, Format: "qcow2",
		SizeBytes:        sql.NullInt64{Int64: int64(len(body)), Valid: true},
		VirtualSizeBytes: sql.NullInt64{Int64: imageDiskGB << 30, Valid: true},
		VolumeName:       sql.NullString{String: imported.ID, Valid: true},
		UpdatedAt:        h.now.Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) input(name string) kubernetes.CreateInput {
	return kubernetes.CreateInput{Name: name, ControlPlaneType: nodeType, NodeType: nodeType, NodeCount: 2}
}

func (h *harness) create(t *testing.T, name string) kubernetes.Cluster {
	t.Helper()
	c, err := h.svc.Create(t.Context(), h.input(name))
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	return c
}

func (h *harness) run(t *testing.T, clusterID string, handler func(context.Context, worker.Task) error) error {
	t.Helper()
	row, err := h.store.GetLatestInstanceTask(t.Context(), sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: clusterID, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetLatestInstanceTask(): %v", err)
	}
	err = handler(t.Context(), worker.Task{ID: row.ID, Type: row.Type, ResourceID: clusterID, Payload: json.RawMessage(row.Payload)})
	params := sqlc.UpdateTaskParams{ID: row.ID, State: string(task.StateSucceeded), Progress: 100,
		Attempts: row.Attempts + 1, UpdatedAt: h.now.Format(time.RFC3339Nano)}
	if err != nil {
		params.State = string(task.StateFailed)
		params.Error = sql.NullString{String: err.Error(), Valid: true}
	}
	_, _ = h.store.UpdateTask(context.WithoutCancel(t.Context()), params)
	return err
}

func (h *harness) get(t *testing.T, clusterID string) kubernetes.Cluster {
	t.Helper()
	c, err := h.svc.Get(t.Context(), clusterID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	return c
}

func (h *harness) active(t *testing.T, name string) kubernetes.Cluster {
	t.Helper()
	h.kubernetesImage(t, "1.37")
	c := h.create(t, name)
	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}
	return h.get(t, c.ID)
}

func TestCreateReservesTheWholeCluster(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	c := h.create(t, "orders")

	if c.State != kubernetes.StateCreating || c.KubernetesVersion != "1.37" || c.TaskID == nil {
		t.Errorf("Create() = %+v, want creating 1.37 with a task", c)
	}
	if c.PodCIDR != "100.64.0.0/16" || c.ServiceCIDR != "100.96.0.0/16" {
		t.Errorf("ranges = %s, %s, want the K9 defaults", c.PodCIDR, c.ServiceCIDR)
	}
	names := []string{}
	for _, n := range c.Nodes {
		names = append(names, n.Role+":"+n.Name)
	}
	if strings.Join(names, " ") != "control-plane:orders-cp-1 worker:orders-default-1 worker:orders-default-2" {
		t.Errorf("nodes = %v", names)
	}
	if len(c.NodeGroups) != 1 || c.NodeGroups[0].DesiredSize != 2 || c.NodeGroups[0].Name != "default" {
		t.Errorf("node groups = %+v", c.NodeGroups)
	}

	// The PKI exists before the task runs, and the CA in the row is its CA.
	pki, err := k8spki.LoadDir(filepath.Join(h.dataDir, "clusters", c.ID))
	if err != nil {
		t.Fatalf("LoadDir(): %v", err)
	}
	if string(pki.CA.CertPEM) != c.CACertificate {
		t.Error("the row's CA is not the one in the PKI directory")
	}
	// The nodes are managed VMs, hidden from the Instances pages.
	for _, n := range c.Nodes {
		row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: n.InstanceID})
		if err != nil || row.ManagedBy != "eks" {
			t.Errorf("node %s: %v managed_by=%q", n.Name, err, row.ManagedBy)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	for name, tc := range map[string]struct {
		mutate func(*kubernetes.CreateInput)
		field  string
	}{
		"an uppercase name":          {func(in *kubernetes.CreateInput) { in.Name = "Orders" }, "name"},
		"a name too long":            {func(in *kubernetes.CreateInput) { in.Name = strings.Repeat("a", 41) }, "name"},
		"no workers":                 {func(in *kubernetes.CreateInput) { in.NodeCount = -1 }, "node_count"},
		"eleven workers":             {func(in *kubernetes.CreateInput) { in.NodeCount = 11 }, "node_count"},
		"a patch version":            {func(in *kubernetes.CreateInput) { in.KubernetesVersion = "1.37.2" }, "kubernetes_version"},
		"a one-CPU control plane":    {func(in *kubernetes.CreateInput) { in.ControlPlaneType = "kumo.small" }, "control_plane_type"},
		"a node with too little RAM": {func(in *kubernetes.CreateInput) { in.NodeType = "kumo.micro" }, "node_type"},
		"an unknown type":            {func(in *kubernetes.CreateInput) { in.NodeType = "kumo.huge" }, "node_type"},
		"a root disk too large":      {func(in *kubernetes.CreateInput) { in.NodeRootDiskGB = 501 }, "node_root_disk_gb"},
		"a bad subnet id":            {func(in *kubernetes.CreateInput) { in.SubnetID = "default" }, "subnet_id"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := h.input("orders")
			tc.mutate(&in)
			_, err := h.svc.Create(t.Context(), in)
			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Create() = %v, want a validation error", err)
			}
			found := false
			for _, f := range invalid.Fields {
				found = found || f.Field == tc.field
			}
			if !found {
				t.Errorf("fields = %+v, want one on %s", invalid.Fields, tc.field)
			}
		})
	}
	assertNoPKILeft(t, h)
}

func TestCreateWithNoImage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if _, err := h.svc.Create(t.Context(), h.input("orders")); !errors.Is(err, kubernetes.ErrNoEngineImage) {
		t.Errorf("Create() = %v, want ErrNoEngineImage", err)
	}
	assertNoPKILeft(t, h)
}

// A cluster the node cannot hold is refused whole, and its PKI goes with it.
func TestCreateBeyondCapacityLeavesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	in := h.input("orders")
	in.NodeCount = 10 // eleven 4 GiB VMs on a 32 GiB node
	if _, err := h.svc.Create(t.Context(), in); !errors.Is(err, service.ErrQuota) {
		t.Fatalf("Create() = %v, want ErrQuota", err)
	}
	var n int
	_ = h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM instances`).Scan(&n)
	if n != 0 {
		t.Errorf("%d instances written by a refused create", n)
	}
	assertNoPKILeft(t, h)
}

func TestCreateRefusesADuplicateName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	in := h.input("orders")
	in.NodeCount = 1
	if _, err := h.svc.Create(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Create(t.Context(), in); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("a second Create() = %v, want ErrAlreadyExists", err)
	}
	entries, _ := os.ReadDir(filepath.Join(h.dataDir, "clusters"))
	if len(entries) != 1 {
		t.Errorf("%d PKI directories, want the first cluster's alone", len(entries))
	}
}

func assertNoPKILeft(t *testing.T, h harness) {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(h.dataDir, "clusters"))
	if len(entries) != 0 {
		t.Errorf("a refused create left %d PKI directories", len(entries))
	}
}

func TestCreateHandlerBuildsTheCluster(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	c := h.active(t, "orders")

	if c.State != kubernetes.StateActive || c.StateReason != nil {
		t.Fatalf("state = %s (%v), want active", c.State, c.StateReason)
	}
	if c.Endpoint == nil || *c.Endpoint != "https://api.orders.default:6443" {
		t.Errorf("endpoint = %v, want api.orders.default", c.Endpoint)
	}
	for _, n := range c.Nodes {
		if n.State != "running" || n.Ready == nil || !*n.Ready {
			t.Errorf("node %s: state %s ready %v, want running and Ready", n.Name, n.State, n.Ready)
		}
		if _, err := h.hv.GetDomain(t.Context(), n.InstanceID); err != nil {
			t.Errorf("node %s has no domain: %v", n.Name, err)
		}
	}

	// The endpoint resolves on the VPC: a DNS record at the control plane.
	info, err := h.hv.GetNetwork(t.Context(), h.vpcID)
	if err != nil {
		t.Fatal(err)
	}
	cpIP := c.Nodes[0].PrivateIP
	if len(info.DNSHosts) != 1 || info.DNSHosts[0].IP != cpIP || info.DNSHosts[0].Hostnames[0] != "api.orders.default" {
		t.Errorf("DNS hosts = %+v, want api.orders.default at %s", info.DNSHosts, cpIP)
	}

	// The workers joined with a token the API minted, and kumod dialed the
	// control plane by address with the endpoint as the TLS name.
	if len(h.api.tokens) != 1 {
		t.Errorf("%d bootstrap tokens minted, want one for the workers", len(h.api.tokens))
	}
	conn := h.api.conns[0]
	if conn.Address != cpIP+":6443" || conn.ServerName != "api.orders.default" {
		t.Errorf("connection = %s / %s", conn.Address, conn.ServerName)
	}

	// A requeued create of a finished cluster does nothing.
	if err := h.svc.CreateHandler(t.Context(), worker.Task{ID: "task-x", Type: task.TypeK8sClusterCreate,
		Payload: json.RawMessage(`{"cluster_id":"` + c.ID + `"}`)}); err != nil {
		t.Errorf("a requeued create = %v", err)
	}
	if len(h.api.tokens) != 1 {
		t.Error("a requeued create of an active cluster minted another token")
	}
}

// A control plane that never answers fails the cluster and keeps the VMs: the
// consoles are the evidence, and delete is the way out.
func TestCreateHandlerFailsWhenTheAPINeverAnswers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	c := h.create(t, "orders")
	h.api.set(false)

	if err := h.run(t, c.ID, h.svc.CreateHandler); err == nil {
		t.Fatal("CreateHandler() succeeded with an API that never answered")
	}
	got := h.get(t, c.ID)
	if got.State != kubernetes.StateFailed || got.StateReason == nil || !strings.Contains(*got.StateReason, "API server") {
		t.Errorf("state = %s (%v), want failed naming the API server", got.State, got.StateReason)
	}
	if _, err := h.hv.GetDomain(t.Context(), got.Nodes[0].InstanceID); err != nil {
		t.Errorf("the control plane's domain was removed: %v", err)
	}
}

// Nodes that never become Ready leave the cluster degraded, not failed: the
// control plane works, and the endpoint is published.
func TestCreateHandlerDegradesWhenNodesAreNotReady(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	c := h.create(t, "orders")
	h.api.set(true, "orders-default-2")

	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}
	got := h.get(t, c.ID)
	if got.State != kubernetes.StateDegraded || got.StateReason == nil || !strings.Contains(*got.StateReason, "orders-default-2") {
		t.Errorf("state = %s (%v), want degraded naming the node", got.State, got.StateReason)
	}
	if got.Endpoint == nil {
		t.Error("a degraded cluster's endpoint was not published")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()

	for _, from := range []string{"active", "failed", "creating"} {
		t.Run("from "+from, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			var c kubernetes.Cluster
			switch from {
			case "active":
				c = h.active(t, "orders")
			case "failed":
				h.kubernetesImage(t, "1.37")
				c = h.create(t, "orders")
				h.api.set(false)
				_ = h.run(t, c.ID, h.svc.CreateHandler)
			case "creating":
				h.kubernetesImage(t, "1.37")
				c = h.create(t, "orders")
			}
			nodes := h.get(t, c.ID).Nodes

			queued, err := h.svc.Delete(t.Context(), c.ID)
			if err != nil || queued.State != kubernetes.StateDeleting || queued.TaskID == nil {
				t.Fatalf("Delete() = %+v, %v", queued, err)
			}
			if _, err := h.svc.Delete(t.Context(), c.ID); !errors.Is(err, service.ErrConflict) {
				t.Errorf("a second Delete() = %v, want a conflict", err)
			}
			if err := h.run(t, c.ID, h.svc.DeleteHandler); err != nil {
				t.Fatalf("DeleteHandler(): %v", err)
			}

			if _, err := h.svc.Get(t.Context(), c.ID); !errors.Is(err, service.ErrNotFound) {
				t.Errorf("Get() after the delete = %v, want ErrNotFound", err)
			}
			for _, n := range nodes {
				if _, err := h.hv.GetDomain(t.Context(), n.InstanceID); !errors.Is(err, hypervisor.ErrNotFound) {
					t.Errorf("node %s's domain is still there", n.Name)
				}
				if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: n.InstanceID}); err == nil {
					t.Errorf("node %s's row is still there", n.Name)
				}
			}
			if info, _ := h.hv.GetNetwork(t.Context(), h.vpcID); len(info.DNSHosts) != 0 {
				t.Errorf("the endpoint record is still there: %+v", info.DNSHosts)
			}
			if _, err := os.Stat(filepath.Join(h.dataDir, "clusters", c.ID)); !os.IsNotExist(err) {
				t.Errorf("the PKI directory is still there: %v", err)
			}
			// A requeued delete of a cluster already gone is harmless.
			if err := h.svc.DeleteHandler(t.Context(), worker.Task{ID: "task-x", Type: task.TypeK8sClusterDelete,
				Payload: json.RawMessage(`{"cluster_id":"` + c.ID + `"}`)}); err != nil {
				t.Errorf("a requeued delete = %v", err)
			}
		})
	}
}

func TestMintKubeconfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")

	for role, group := range map[auth.Role]string{auth.RoleAdmin: k8spki.GroupAdmins, auth.RoleViewer: k8spki.GroupViewers} {
		kc, err := h.svc.MintKubeconfig(t.Context(), c.ID, "ahmed", role)
		if err != nil {
			t.Fatalf("MintKubeconfig(%s): %v", role, err)
		}
		var doc struct {
			Clusters []struct {
				Cluster struct {
					Server string `yaml:"server"`
				} `yaml:"cluster"`
			} `yaml:"clusters"`
			Users []struct {
				User struct {
					Cert string `yaml:"client-certificate-data"`
				} `yaml:"user"`
			} `yaml:"users"`
		}
		if err := yaml.Unmarshal(kc.Kubeconfig.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Clusters[0].Cluster.Server != "https://api.orders.default:6443" {
			t.Errorf("server = %s", doc.Clusters[0].Cluster.Server)
		}
		certPEM, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Cert)
		block, _ := pem.Decode(certPEM)
		cert, _ := x509.ParseCertificate(block.Bytes)
		if cert.Subject.CommonName != "ahmed" || cert.Subject.Organization[0] != group || kc.Group != group {
			t.Errorf("%s got %v (%s), want group %s", role, cert.Subject, kc.Group, group)
		}
	}

	// Not before the cluster is up.
	in := h.input("billing")
	in.NodeCount = 1
	creating, err := h.svc.Create(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.MintKubeconfig(t.Context(), creating.ID, "ahmed", auth.RoleAdmin); !errors.Is(err, service.ErrConflict) {
		t.Errorf("MintKubeconfig() of a creating cluster = %v, want a conflict", err)
	}
}

// The health pass degrades a cluster after three failed passes in a row, keeps
// the reason current, and brings it back with one healthy pass.
func TestHealthPass(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")

	pass := func() kubernetes.Cluster {
		*h.now = h.now.Add(2 * time.Minute) // past the probe interval
		if _, err := h.svc.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile(): %v", err)
		}
		return h.get(t, c.ID)
	}

	h.api.set(false)
	for i := 1; i <= 2; i++ {
		if got := pass(); got.State != kubernetes.StateActive {
			t.Fatalf("after %d failed passes state = %s, want still active", i, got.State)
		}
	}
	got := pass()
	if got.State != kubernetes.StateDegraded || got.StateReason == nil || !strings.Contains(*got.StateReason, "API server") {
		t.Fatalf("after 3 failed passes = %s (%v), want degraded", got.State, got.StateReason)
	}

	h.api.set(true, "orders-default-1")
	got = pass()
	if got.State != kubernetes.StateDegraded || !strings.Contains(*got.StateReason, "orders-default-1") {
		t.Errorf("reason = %v, want it to name the node that is not Ready now", got.StateReason)
	}

	h.api.set(true)
	if got := pass(); got.State != kubernetes.StateActive || got.StateReason != nil {
		t.Errorf("after a healthy pass = %s (%v), want active", got.State, got.StateReason)
	}

	// Within the probe interval nothing is asked again.
	before := len(h.api.conns)
	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(h.api.conns) != before {
		t.Error("the health pass probed a cluster twice within the interval")
	}
}

func TestListPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.kubernetesImage(t, "1.37")
	for _, name := range []string{"a", "b", "c"} {
		in := h.input(name)
		in.NodeCount = 1
		if _, err := h.svc.Create(t.Context(), in); err != nil {
			t.Fatal(err)
		}
		*h.now = h.now.Add(time.Second)
	}
	page, err := h.svc.List(t.Context(), kubernetes.ListInput{PageSize: 2})
	if err != nil || len(page.Items) != 2 || page.NextPageToken == nil || page.Items[0].Name != "c" {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	next, err := h.svc.List(t.Context(), kubernetes.ListInput{PageSize: 2, PageToken: *page.NextPageToken})
	if err != nil || len(next.Items) != 1 || next.Items[0].Name != "a" || next.NextPageToken != nil {
		t.Errorf("second page = %+v, %v", next, err)
	}
	if _, err := h.svc.List(t.Context(), kubernetes.ListInput{State: "running"}); err == nil {
		t.Error("an unknown state filter was accepted")
	}
}

// On the fake hypervisor the module works end to end with FakeAPI.
func TestFakeAPIRunsAWholeCreate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	svc, err := kubernetes.New(kubernetes.Options{
		Logger: logging.Discard(), Store: h.store, Hypervisor: h.hv, Instances: h.instances,
		Tasks: h.tasks, DataDir: h.dataDir, API: kubernetes.FakeAPI,
		ReadyTimeout: time.Second, PollInterval: time.Millisecond, Now: func() time.Time { return *h.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	h.svc = svc
	c := h.active(t, "orders")
	if c.State != kubernetes.StateActive {
		t.Errorf("state = %s, want active on the fake", c.State)
	}
}
