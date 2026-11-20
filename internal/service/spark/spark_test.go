package spark_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	objfake "github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/spark"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/sparkapi"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"
	imageDiskGB   = 1
	s3Endpoint    = "http://192.168.1.10:9000"
)

// master is a scripted Spark master: alive or not, and which worker
// addresses it reports ALIVE.
type master struct {
	mu      sync.Mutex
	alive   bool
	down    map[string]bool // worker IPs not registered
	answers int
}

func (m *master) factory(conn spark.Connection) (spark.StatusAPI, error) {
	return &scripted{m: m, conn: conn}, nil
}

type scripted struct {
	m    *master
	conn spark.Connection
}

var errNoAnswer = errors.New("connection refused")

func (s *scripted) Status(context.Context) (sparkapi.Status, error) {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	s.m.answers++
	if !s.m.alive {
		return sparkapi.Status{}, errNoAnswer
	}
	st := sparkapi.Status{Status: sparkapi.MasterAlive}
	for _, ip := range s.conn.WorkerIPs {
		state := sparkapi.WorkerAlive
		if s.m.down[ip] {
			state = sparkapi.WorkerDead
		}
		st.Workers = append(st.Workers, sparkapi.Worker{Host: ip, State: state})
	}
	return st, nil
}
func (s *scripted) Close() {}

func (m *master) set(alive bool, down ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alive = alive
	m.down = map[string]bool{}
	for _, ip := range down {
		m.down[ip] = true
	}
}

type harness struct {
	svc       *spark.Service
	instances *instance.Service
	tasks     *task.Service
	store     *store.Store
	hv        *fake.Fake
	objects   *objfake.Store
	images    *image.Service
	api       *master
	dataDir   string
	logs      *bytes.Buffer
	now       *time.Time
}

type options struct {
	objects    bool
	s3Endpoint string
}

func newHarness(t *testing.T, opt ...options) harness {
	t.Helper()
	o := options{objects: true, s3Endpoint: s3Endpoint}
	if len(opt) > 0 {
		o = opt[0]
	}
	ctx := t.Context()
	db := storetest.New(t)
	hv := fake.New(fake.Config{Capacity: hypervisor.Capacity{VCPUs: 16, MemoryBytes: 64 << 30, DiskBytes: 500 << 30}})
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool: imagesPool, InstancesPool: instancesPool, Network: "kumo-default", DataDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	vpcID := seedNetwork(t, db, hv)

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
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

	objects := objfake.New()
	var store objectstore.ObjectStore
	if o.objects {
		store = objects
	}
	logs := &bytes.Buffer{}
	m := &master{alive: true, down: map[string]bool{}}
	dataDir := t.TempDir()
	svc, err := spark.New(spark.Options{
		Logger: slog.New(slog.NewJSONHandler(&lockedWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Store:  db, Hypervisor: hv, Instances: instances, Tasks: tasks, ObjectStore: store,
		S3Endpoint: o.s3Endpoint, DataDir: dataDir, API: m.factory,
		ReadyTimeout: 100 * time.Millisecond, PollInterval: time.Millisecond, ProbeInterval: time.Minute,
		Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return harness{svc: svc, instances: instances, tasks: tasks, store: db, hv: hv, objects: objects, images: images, api: m,
		dataDir: dataDir, logs: logs, now: &now}
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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

// sparkImage publishes an available spark engine image the way the import
// workflow would.
func (h *harness) sparkImage(t *testing.T, version string) {
	t.Helper()
	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name: "spark-" + version, SourceURL: "https://example.invalid/spark.qcow2.gz", DefaultUser: "ubuntu",
		MinDiskGB: imageDiskGB, Kind: image.KindEngine, Engine: image.EngineSpark, EngineVersion: version,
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	body := qcow2test.Tiny(imageDiskGB << 30)
	path := filepath.Join(t.TempDir(), "spark.qcow2")
	_ = os.WriteFile(path, body, 0o600)
	file, _ := os.Open(path) //nolint:gosec // the test's own file
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

func (h *harness) bucket(t *testing.T, name string) {
	t.Helper()
	if err := h.objects.CreateBucket(t.Context(), name); err != nil {
		t.Fatal(err)
	}
}

func input(name string) spark.CreateInput {
	return spark.CreateInput{Name: name, ObjectStorage: true, Buckets: []string{"data", "logs"},
		EventLogBucket: "logs", WorkerType: "kumo.medium"}
}

func (h *harness) create(t *testing.T, in spark.CreateInput) spark.Cluster {
	t.Helper()
	c, err := h.svc.Create(t.Context(), in)
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
	return handler(t.Context(), worker.Task{ID: row.ID, Type: row.Type, ResourceID: clusterID, Payload: json.RawMessage(row.Payload)})
}

func (h *harness) get(t *testing.T, clusterID string) spark.Cluster {
	t.Helper()
	c, err := h.svc.Get(t.Context(), clusterID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	return c
}

// ready is a harness with the image and the buckets a default create needs.
func ready(t *testing.T) harness {
	t.Helper()
	h := newHarness(t)
	h.sparkImage(t, "4.3")
	h.bucket(t, "data")
	h.bucket(t, "logs")
	return h
}

func (h *harness) active(t *testing.T, name string) spark.Cluster {
	t.Helper()
	c := h.create(t, input(name))
	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}
	return h.get(t, c.ID)
}

func (h *harness) seedOf(t *testing.T, instanceID string) string {
	t.Helper()
	for _, v := range h.hv.Volumes() {
		if v.Name == hypervisor.SeedISOName(instanceID) {
			return v.Content
		}
	}
	t.Fatalf("no seed for %s", instanceID)
	return ""
}

func (h *harness) accessKeyOf(t *testing.T, clusterID string) string {
	t.Helper()
	row, err := h.store.GetSparkCluster(t.Context(), sqlc.GetSparkClusterParams{ID: clusterID})
	if err != nil || !row.AccessKeyID.Valid {
		t.Fatalf("the cluster has no access key: %v", err)
	}
	return row.AccessKeyID.String
}

func TestCreateReservesTheWholeCluster(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.create(t, input("etl"))

	if c.State != spark.StateCreating || c.TaskID == nil || len(c.Nodes) != 3 {
		t.Fatalf("created = %+v", c)
	}
	if c.Nodes[0].Name != "etl-master" || c.Nodes[0].Role != spark.RoleMaster ||
		c.Nodes[1].Name != "etl-default-1" || c.Nodes[2].Name != "etl-default-2" {
		t.Errorf("nodes = %+v", c.Nodes)
	}
	if !c.ObjectStorage || strings.Join(c.Buckets, ",") != "data,logs" || c.EventLogBucket == nil || *c.EventLogBucket != "logs" {
		t.Errorf("object storage = %v %v %v", c.ObjectStorage, c.Buckets, c.EventLogBucket)
	}
	// The VMs are managed rows, hidden from the Instances pages.
	for _, n := range c.Nodes {
		row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: n.InstanceID})
		if err != nil || row.ManagedBy != string(instance.ManagedBySpark) {
			t.Errorf("%s: managed_by %q, %v", n.Name, row.ManagedBy, err)
		}
	}
	// The access key exists, read-write on the cluster's buckets only.
	_, grant, ok := h.objects.AccessKey(h.accessKeyOf(t, c.ID))
	if !ok || grant.Level != objectstore.AccessReadWrite || strings.Join(grant.Buckets, ",") != "data,logs" {
		t.Errorf("access key grant = %+v (%v)", grant, ok)
	}
	// The secrets, 0600 in a 0700 directory, outside the database.
	dir := filepath.Join(h.dataDir, "spark", c.ID)
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("secrets dir: %v %v", info, err)
	}
	for _, f := range []string{"spark.secret", "s3.secret"} {
		if info, err := os.Stat(filepath.Join(dir, f)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, info, err)
		}
	}
}

func TestCreateRefusals(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		edit func(*spark.CreateInput)
		want error
	}{
		"a bad name":              {func(in *spark.CreateInput) { in.Name = "ETL" }, nil},
		"eleven workers":          {func(in *spark.CreateInput) { in.WorkerCount = 11 }, nil},
		"a bad version":           {func(in *spark.CreateInput) { in.SparkVersion = "4.3.0" }, nil},
		"buckets, no storage":     {func(in *spark.CreateInput) { in.ObjectStorage = false; in.EventLogBucket = "" }, nil},
		"event log, no storage":   {func(in *spark.CreateInput) { in.ObjectStorage = false; in.Buckets = nil }, nil},
		"event log not a bucket":  {func(in *spark.CreateInput) { in.EventLogBucket = "other" }, nil},
		"storage, no buckets":     {func(in *spark.CreateInput) { in.Buckets = nil; in.EventLogBucket = "" }, nil},
		"a bucket twice":          {func(in *spark.CreateInput) { in.Buckets = []string{"data", "data"}; in.EventLogBucket = "" }, nil},
		"a missing bucket":        {func(in *spark.CreateInput) { in.Buckets = []string{"nope"}; in.EventLogBucket = "" }, nil},
		"a small master":          {func(in *spark.CreateInput) { in.MasterType = "kumo.small" }, nil},
		"a tiny worker":           {func(in *spark.CreateInput) { in.WorkerType = "kumo.micro" }, nil},
		"an unknown type":         {func(in *spark.CreateInput) { in.WorkerType = "kumo.huge" }, nil},
		"a version with no image": {func(in *spark.CreateInput) { in.SparkVersion = "4.5" }, spark.ErrNoEngineImage},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := ready(t)
			in := input("etl")
			tc.edit(&in)
			_, err := h.svc.Create(t.Context(), in)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Errorf("Create() = %v, want %v", err, tc.want)
				}
			} else {
				var invalid *service.ValidationError
				if !errors.As(err, &invalid) {
					t.Errorf("Create() = %v, want a validation error", err)
				}
			}
			assertNothingLeft(t, h)
		})
	}
}

// Nothing a refused create made survives it: no key, no secrets, no rows.
func assertNothingLeft(t *testing.T, h harness) {
	t.Helper()
	if keys, _ := h.objects.ListAccessKeys(t.Context()); len(keys) != 0 {
		t.Errorf("a refused create left access keys %v", keys)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.dataDir, "spark")); len(entries) != 0 {
		t.Errorf("a refused create left %d secrets directories", len(entries))
	}
	if page, _ := h.svc.List(t.Context(), spark.ListInput{}); len(page.Items) != 0 {
		t.Errorf("a refused create left %d clusters", len(page.Items))
	}
}

func TestCreateWithObjectStorageNodeCannotGive(t *testing.T) {
	t.Parallel()
	for name, o := range map[string]options{
		"object storage off":  {objects: false, s3Endpoint: s3Endpoint},
		"a loopback endpoint": {objects: true, s3Endpoint: "http://127.0.0.1:9000"},
		"no endpoint":         {objects: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, o)
			h.sparkImage(t, "4.3")
			h.bucket(t, "data")
			h.bucket(t, "logs")
			if _, err := h.svc.Create(t.Context(), input("etl")); err == nil {
				t.Fatal("Create() succeeded")
			}
			assertNothingLeft(t, h)
			// Without object storage the same node creates a cluster.
			in := spark.CreateInput{Name: "plain", WorkerType: "kumo.medium"}
			if _, err := h.svc.Create(t.Context(), in); err != nil {
				t.Errorf("Create() without object storage: %v", err)
			}
		})
	}
}

func TestCreateRefusesADuplicateNameAndCleansUp(t *testing.T) {
	t.Parallel()
	h := ready(t)
	first := h.create(t, input("etl"))
	if _, err := h.svc.Create(t.Context(), input("etl")); !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("a second etl = %v, want ErrAlreadyExists", err)
	}
	keys, _ := h.objects.ListAccessKeys(t.Context())
	if len(keys) != 1 || keys[0] != h.accessKeyOf(t, first.ID) {
		t.Errorf("access keys = %v, want the first cluster's alone", keys)
	}
	if entries, _ := os.ReadDir(filepath.Join(h.dataDir, "spark")); len(entries) != 1 {
		t.Errorf("%d secrets directories, want the first cluster's", len(entries))
	}
}

func TestCreateHandlerBuildsTheCluster(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")

	if c.State != spark.StateActive || c.Endpoint == nil || *c.Endpoint != "spark://etl.default:7077" {
		t.Fatalf("cluster = %s %v", c.State, c.Endpoint)
	}
	if c.MasterUIURL == nil || *c.MasterUIURL != "http://etl.default:8080" ||
		c.HistoryURL == nil || *c.HistoryURL != "http://etl.default:18080" {
		t.Errorf("UIs = %v %v", c.MasterUIURL, c.HistoryURL)
	}
	for _, n := range c.Nodes {
		if n.State != "running" || n.Alive == nil || !*n.Alive {
			t.Errorf("%s is %s, alive %v", n.Name, n.State, n.Alive)
		}
	}
	// The endpoint record and the event-log marker.
	if _, err := h.objects.HeadObject(t.Context(), "logs", "spark-events/"+c.ID+"/.keep"); err != nil {
		t.Errorf("the event-log marker is missing: %v", err)
	}
	// The master's seed runs the history server with the bucket; a worker's
	// is sized from its type (kumo.medium: 2 vCPU, 4096 MiB less 1024) and
	// carries no S3 credential.
	masterSeed := h.seedOf(t, c.Nodes[0].InstanceID)
	if !strings.Contains(masterSeed, "KUMO_SPARK_HISTORY=1") ||
		!strings.Contains(masterSeed, "s3a://logs/spark-events/"+c.ID) {
		t.Errorf("the master's seed does not run the history server on the bucket")
	}
	workerSeed := h.seedOf(t, c.Nodes[1].InstanceID)
	if !strings.Contains(workerSeed, "SPARK_WORKER_CORES=2") || !strings.Contains(workerSeed, "SPARK_WORKER_MEMORY=3072m") ||
		!strings.Contains(workerSeed, "SPARK_MASTER_URL=spark://"+c.Nodes[0].PrivateIP+":7077") {
		t.Errorf("the worker's seed is not sized from its type")
	}
	if strings.Contains(workerSeed, "fs.s3a") {
		t.Error("a worker's seed carries S3 settings")
	}
	// No secret in the log, and none in a task payload.
	spk, s3, _ := readSecrets(t, h, c.ID)
	for _, s := range []string{spk, s3} {
		if strings.Contains(h.logs.String(), s) {
			t.Error("the log carries a secret")
		}
		var payloads []string
		rows, _ := h.store.DB().QueryContext(t.Context(), `SELECT payload FROM tasks`)
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			payloads = append(payloads, p)
		}
		_ = rows.Close()
		if strings.Contains(strings.Join(payloads, " "), s) {
			t.Error("a task payload carries a secret")
		}
	}
}

func readSecrets(t *testing.T, h harness, clusterID string) (string, string, error) {
	t.Helper()
	dir := filepath.Join(h.dataDir, "spark", clusterID)
	a, err := os.ReadFile(filepath.Join(dir, "spark.secret")) //nolint:gosec // the test's own directory
	if err != nil {
		return "", "", err
	}
	b, _ := os.ReadFile(filepath.Join(dir, "s3.secret")) //nolint:gosec // the test's own directory
	return string(a), string(b), nil
}

func TestClusterWithoutObjectStorageHasNoHistoryServer(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	h.sparkImage(t, "4.3")
	c := h.create(t, spark.CreateInput{Name: "plain", WorkerType: "kumo.medium", WorkerCount: 1})
	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatal(err)
	}
	got := h.get(t, c.ID)
	if got.State != spark.StateActive || got.HistoryURL != nil || got.ObjectStorage {
		t.Errorf("cluster = %s, history %v, object storage %v", got.State, got.HistoryURL, got.ObjectStorage)
	}
	if !strings.Contains(h.seedOf(t, got.Nodes[0].InstanceID), "KUMO_SPARK_HISTORY=0") {
		t.Error("the master runs a history server with nowhere to read")
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "spark", c.ID, "s3.secret")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an S3 secret exists for a cluster without object storage: %v", err)
	}
}

func TestCreateHandlerDegradesWhenAWorkerNeverRegisters(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.create(t, input("etl"))
	h.api.set(true, c.Nodes[2].PrivateIP)
	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}
	got := h.get(t, c.ID)
	if got.State != spark.StateDegraded || got.StateReason == nil || !strings.Contains(*got.StateReason, "etl-default-2") {
		t.Errorf("state = %s (%v), want degraded naming etl-default-2", got.State, got.StateReason)
	}
	if got.Endpoint == nil {
		t.Error("a degraded cluster has no endpoint: its master works")
	}
}

func TestCreateHandlerFailsWhenTheMasterNeverAnswers(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.create(t, input("etl"))
	h.api.set(false)
	if err := h.run(t, c.ID, h.svc.CreateHandler); err == nil {
		t.Fatal("CreateHandler() succeeded with a master that never answered")
	}
	got := h.get(t, c.ID)
	if got.State != spark.StateFailed || got.StateReason == nil || !strings.Contains(*got.StateReason, "master") {
		t.Errorf("state = %s (%v), want failed naming the master", got.State, got.StateReason)
	}
	if _, err := h.hv.GetDomain(t.Context(), got.Nodes[0].InstanceID); err != nil {
		t.Errorf("the master's domain was removed: %v", err)
	}
}

func TestDeleteTakesEverythingButTheBuckets(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")
	key := h.accessKeyOf(t, c.ID)

	queued, err := h.svc.Delete(t.Context(), c.ID)
	if err != nil || queued.State != spark.StateDeleting {
		t.Fatalf("Delete() = %s, %v", queued.State, err)
	}
	if _, err := h.svc.Delete(t.Context(), c.ID); !errors.Is(err, service.ErrConflict) {
		t.Errorf("a second Delete() = %v, want a conflict", err)
	}
	if err := h.run(t, c.ID, h.svc.DeleteHandler); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}
	if _, err := h.svc.Get(t.Context(), c.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() after the delete = %v", err)
	}
	for _, n := range c.Nodes {
		if _, err := h.hv.GetDomain(t.Context(), n.InstanceID); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("%s's domain survived", n.Name)
		}
		if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: n.InstanceID}); err == nil {
			t.Errorf("%s's row survived", n.Name)
		}
	}
	if _, _, ok := h.objects.AccessKey(key); ok {
		t.Error("the access key survived")
	}
	if _, err := os.Stat(filepath.Join(h.dataDir, "spark", c.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the secrets survived: %v", err)
	}
	for _, b := range []string{"data", "logs"} {
		if _, err := h.objects.HeadBucket(t.Context(), b); err != nil {
			t.Errorf("bucket %s went with the cluster: %v", b, err)
		}
	}
	// A requeued delete of a cluster already gone is fine.
	if err := h.run(t, c.ID, h.svc.DeleteHandler); err != nil {
		t.Errorf("a requeued DeleteHandler() = %v", err)
	}
}

func TestHealthPass(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")
	pass := func() {
		t.Helper()
		*h.now = h.now.Add(2 * time.Minute)
		if _, err := h.svc.Reconcile(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	h.api.set(true, c.Nodes[1].PrivateIP)
	pass()
	pass()
	if got := h.get(t, c.ID); got.State != spark.StateActive {
		t.Fatalf("degraded after two bad passes: %s", got.State)
	}
	pass()
	got := h.get(t, c.ID)
	if got.State != spark.StateDegraded || !strings.Contains(*got.StateReason, "etl-default-1") {
		t.Fatalf("after three bad passes: %s (%v)", got.State, got.StateReason)
	}
	h.api.set(false)
	pass()
	if got := h.get(t, c.ID); !strings.Contains(*got.StateReason, "master does not answer") {
		t.Errorf("the reason did not follow the cause: %v", got.StateReason)
	}
	h.api.set(true)
	pass()
	if got := h.get(t, c.ID); got.State != spark.StateActive || got.StateReason != nil {
		t.Errorf("after a good pass: %s (%v)", got.State, got.StateReason)
	}
}

func TestClientConfig(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.create(t, input("etl"))
	if _, err := h.svc.ClientConfig(t.Context(), c.ID); !errors.Is(err, service.ErrConflict) {
		t.Errorf("ClientConfig() while creating = %v, want a conflict", err)
	}
	if err := h.run(t, c.ID, h.svc.CreateHandler); err != nil {
		t.Fatal(err)
	}
	conf, err := h.svc.ClientConfig(t.Context(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	spk, s3, _ := readSecrets(t, h, c.ID)
	for _, want := range []string{
		"spark://etl.default:7077", spk, s3, h.accessKeyOf(t, c.ID), s3Endpoint,
		"s3a://logs/spark-events/" + c.ID, "spark.driver.host",
	} {
		if !strings.Contains(conf.Value(), want) {
			t.Errorf("the client configuration lacks %.20q", want)
		}
	}
	if strings.Contains(h.logs.String(), spk) || strings.Contains(h.logs.String(), s3) {
		t.Error("issuing the client configuration logged a secret")
	}
	if _, err := h.svc.ClientConfig(t.Context(), "spk-0123456789abcdef0"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("ClientConfig() of no cluster = %v", err)
	}
}

func TestListAndRefusals(t *testing.T) {
	t.Parallel()
	h := ready(t)
	for _, name := range []string{"a", "b", "c"} {
		in := input(name)
		in.WorkerCount = 1
		h.create(t, in)
		*h.now = h.now.Add(time.Second)
	}
	page, err := h.svc.List(t.Context(), spark.ListInput{PageSize: 2})
	if err != nil || len(page.Items) != 2 || page.Items[0].Name != "c" || page.NextPageToken == nil {
		t.Fatalf("List() = %+v, %v", page, err)
	}
	next, err := h.svc.List(t.Context(), spark.ListInput{PageSize: 2, PageToken: *page.NextPageToken})
	if err != nil || len(next.Items) != 1 || next.Items[0].Name != "a" || next.NextPageToken != nil {
		t.Errorf("the second page = %+v, %v", next, err)
	}
	var invalid *service.ValidationError
	for _, in := range []spark.ListInput{{State: "running"}, {PageSize: 500}, {PageToken: "x"}} {
		if _, err := h.svc.List(t.Context(), in); !errors.As(err, &invalid) {
			t.Errorf("List(%+v) = %v, want a validation error", in, err)
		}
	}
	if _, err := h.svc.Get(t.Context(), "eks-0123456789abcdef0"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() of another module's id = %v", err)
	}
}

// FakeAPI is what makes the module usable on the fake hypervisor: a whole
// create goes through it.
func TestFakeAPIRunsAWholeCreate(t *testing.T) {
	t.Parallel()
	h := ready(t)
	svc, err := spark.New(spark.Options{
		Logger: logging.Discard(), Store: h.store, Hypervisor: h.hv, Instances: h.instances,
		Tasks: h.tasks, ObjectStore: h.objects, S3Endpoint: s3Endpoint, DataDir: h.dataDir,
		API: spark.FakeAPI, ReadyTimeout: time.Second, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.Create(t.Context(), input("etl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.run(t, c.ID, svc.CreateHandler); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.Get(t.Context(), c.ID); got.State != spark.StateActive {
		t.Errorf("state = %s, want active", got.State)
	}
}
