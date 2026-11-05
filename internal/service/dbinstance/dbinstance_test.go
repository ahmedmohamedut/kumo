package dbinstance_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"
	instanceType  = "kumo.small"

	// engineDiskGB is the virtual size of the fixture engine image, and so the
	// root disk every DB instance in these tests boots from.
	engineDiskGB = 1
	// password is a fixture admin password: long enough for the floor and
	// printable ASCII, which is a correctness rule rather than a convenience
	// (internal/service/dbcred).
	password = "sup3r-secret-password"
)

var testClock = func() time.Time {
	return time.Date(2026, 5, 6, 10, 0, 0, 0, time.UTC)
}

// errBackendSaidNo is what the fake is told to return at the step under test.
var errBackendSaidNo = errors.New("the backend said no")

// prober is a scriptable readiness probe. Nothing in these tests listens on a
// port, so the probe is the one place a test says whether the engine answers.
type prober struct {
	mu        sync.Mutex
	answer    bool
	calls     int
	addresses []string
	// answerAfter makes the probe fail until it has been called this many
	// times, which is how a first boot that takes a while is modeled.
	answerAfter int
}

func (p *prober) probe(_ context.Context, address string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls++
	p.addresses = append(p.addresses, address)
	if p.answer && p.calls >= p.answerAfter {
		return nil
	}
	return errBackendSaidNo
}

func (p *prober) set(answer bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.answer = answer
}

func (p *prober) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *prober) lastAddress() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.addresses) == 0 {
		return ""
	}
	return p.addresses[len(p.addresses)-1]
}

// harness is the service under test with its collaborators.
type harness struct {
	svc       *dbinstance.Service
	instances *instance.Service
	store     *store.Store
	hv        *fake.Fake
	images    *image.Service
	tasks     *task.Service
	locks     *locks.Set
	probe     *prober
	opts      dbinstance.Options
	vpcID     string
	subnetID  string
	console   string
	now       *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()
	return newHarnessWith(t, fake.Config{}, dbinstance.Options{})
}

// newHarnessWith builds a harness around a configured fake and the options a
// test needs to move.
func newHarnessWith(t *testing.T, hvConfig fake.Config, opts dbinstance.Options) harness {
	t.Helper()

	ctx := t.Context()
	db := storetest.New(t)

	hv := fake.New(hvConfig)
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool:    imagesPool,
		InstancesPool: instancesPool,
		Network:       "kumo-default",
		DataDir:       t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}
	vpcID, subnetID := seedDefaultNetwork(t, db, hv)

	now := testClock()
	clock := func() time.Time { return now }

	types, err := instancetype.New(instancetype.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("instancetype.New(): %v", err)
	}
	tasks, err := task.New(task.Options{Store: db, Now: clock})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	images, err := image.New(image.Options{
		Logger:     logging.Discard(),
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Downloader: imagefetch.New(imagefetch.Options{MaxBytes: 1 << 20}),
		ImagesPool: imagesPool,
		TempDir:    t.TempDir(),
		MaxBytes:   4 << 20,
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}
	pairs, err := keypair.New(keypair.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("keypair.New(): %v", err)
	}

	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	sys := system.New(logging.Discard(), "test", "fake", db, hv, db)
	sys.SetLimits(limits)

	console := t.TempDir()
	instanceLocks := locks.New()
	instances, err := instance.New(instance.Options{
		Logger:        logging.Discard(),
		Store:         db,
		Hypervisor:    hv,
		Volumes:       hv,
		Tasks:         tasks,
		Images:        images,
		Types:         types,
		KeyPairs:      pairs,
		Capacity:      sys,
		InstancesPool: instancesPool,
		ImagesPool:    imagesPool,
		ConsoleDir:    console,
		Network:       vpcID,
		Locks:         instanceLocks,
		Limits:        limits,
		PollInterval:  time.Millisecond,
		PollTimeout:   time.Second,
		StopTimeout:   100 * time.Millisecond,
		Now:           clock,
	})
	if err != nil {
		t.Fatalf("instance.New(): %v", err)
	}

	probe := &prober{answer: true}
	if opts.Probe == nil {
		opts.Probe = probe.probe
	}
	// A real create polls every ten seconds for up to ten minutes, which no
	// test wants to sit through; a test that cares about the bound sets it.
	if opts.ProbeInterval == 0 {
		opts.ProbeInterval = time.Millisecond
	}
	if opts.ReadyTimeout == 0 {
		opts.ReadyTimeout = 200 * time.Millisecond
	}
	dbLocks := locks.New()
	if opts.Locks == nil {
		opts.Locks = dbLocks
	}
	opts.Logger = logging.Discard()
	opts.Store = db
	opts.Hypervisor = hv
	opts.Instances = instances
	opts.Tasks = tasks
	opts.InstancesPool = instancesPool
	opts.Now = clock

	svc, err := dbinstance.New(opts)
	if err != nil {
		t.Fatalf("dbinstance.New(): %v", err)
	}

	return harness{
		svc:       svc,
		opts:      opts,
		instances: instances,
		store:     db,
		hv:        hv,
		images:    images,
		tasks:     tasks,
		locks:     opts.Locks,
		probe:     probe,
		vpcID:     vpcID,
		subnetID:  subnetID,
		console:   console,
		now:       &now,
	}
}

// seedDefaultNetwork stands in for the bootstrap: a default VPC row, its
// default subnet and the fake network behind them, so a reservation resolves
// them exactly as serve.go would. The VPC's name is the DNS domain dnsmasq
// serves, and so the second half of every endpoint these tests assert on.
func seedDefaultNetwork(t *testing.T, db *store.Store, hv *fake.Fake) (vpcID, subnetID string) {
	t.Helper()

	vpcID = id.VPC()
	subnetID = id.Subnet()

	if _, err := db.Exec(t.Context(),
		`INSERT INTO vpcs (id, name, cidr, mode, gateway_ip, netmask, bridge_name,
		   libvirt_network, is_default, created_at, updated_at)
		 VALUES (?1, 'default', '192.168.100.0/24', 'nat', '192.168.100.1', '255.255.255.0', ?2, ?1, 1,
		        '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		vpcID, hypervisor.BridgeName(vpcID)); err != nil {
		t.Fatalf("seed the default VPC: %v", err)
	}
	if _, err := db.Exec(t.Context(),
		`INSERT INTO subnets (id, vpc_id, name, cidr, is_default, created_at, updated_at)
		 VALUES (?1, ?2, 'default', '192.168.100.0/25', 1, '2026-01-02T09:00:00Z', '2026-01-02T09:00:00Z')`,
		subnetID, vpcID); err != nil {
		t.Fatalf("seed the default subnet: %v", err)
	}

	if err := hv.DefineNetwork(t.Context(), hypervisor.NetworkSpec{
		Name: vpcID, Bridge: hypervisor.BridgeName(vpcID),
		CIDR: "192.168.100.0/24", Gateway: "192.168.100.1",
		Mode: hypervisor.NetworkModeNAT, Domain: "default",
	}); err != nil {
		t.Fatalf("DefineNetwork(): %v", err)
	}
	if err := hv.StartNetwork(t.Context(), vpcID); err != nil {
		t.Fatalf("StartNetwork(): %v", err)
	}
	return vpcID, subnetID
}

// engineImage publishes an available engine image, by writing the row and the
// pool volume the way the import workflow would: the import is not what these
// tests are about, and the Kind/Engine fields are M7.2's.
func (h *harness) engineImage(t *testing.T, name, version string) image.Image {
	t.Helper()
	return h.engineImageWithMin(t, name, version, engineDiskGB)
}

func (h *harness) engineImageWithMin(t *testing.T, name, version string, minGB int) image.Image {
	t.Helper()

	imported, err := h.images.Import(t.Context(), image.ImportInput{
		Name:          name,
		SourceURL:     "https://example.invalid/postgres-18.qcow2",
		DefaultUser:   "kumo",
		MinDiskGB:     minGB,
		Kind:          image.KindEngine,
		Engine:        image.EnginePostgres,
		EngineVersion: version,
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}

	body := qcow2test.Tiny(engineDiskGB << 30)
	path := filepath.Join(t.TempDir(), "engine.qcow2")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write the fixture image: %v", err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the fixture image: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only handle

	if err := h.hv.ImportImage(t.Context(), imported.ID, file, hypervisor.ImportSpec{
		SizeBytes:        int64(len(body)),
		VirtualSizeBytes: engineDiskGB << 30,
		Format:           hypervisor.FormatQCOW2,
	}); err != nil {
		t.Fatalf("ImportImage(): %v", err)
	}
	if _, err := h.store.FinalizeImage(t.Context(), sqlc.FinalizeImageParams{
		ID:               imported.ID,
		Format:           string(hypervisor.FormatQCOW2),
		SizeBytes:        sql.NullInt64{Int64: int64(len(body)), Valid: true},
		VirtualSizeBytes: sql.NullInt64{Int64: engineDiskGB << 30, Valid: true},
		VolumeName:       sql.NullString{String: imported.ID, Valid: true},
		UpdatedAt:        h.now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("FinalizeImage(): %v", err)
	}

	got, err := h.images.Get(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("Get(%q): %v", imported.ID, err)
	}
	return got
}

// create asks for a DB instance with the fixture defaults.
func (h *harness) create(t *testing.T, identifier string) dbinstance.DBInstance {
	t.Helper()

	created, err := h.svc.Create(t.Context(), h.createInput(identifier))
	if err != nil {
		t.Fatalf("Create(%q): %v", identifier, err)
	}
	return created
}

func (h *harness) createInput(identifier string) dbinstance.CreateInput {
	return dbinstance.CreateInput{
		Identifier:    identifier,
		Engine:        dbinstance.EnginePostgres,
		EngineVersion: "18",
		InstanceType:  instanceType,
		StorageGB:     20,
		AdminUsername: "app",
		AdminPassword: password,
		DatabaseName:  "orders",
		CreatedBy:     "",
	}
}

// available creates a DB instance and runs its create task to completion, which
// is the state most lifecycle assertions start from.
func (h *harness) available(t *testing.T, identifier string) dbinstance.DBInstance {
	t.Helper()

	h.engineImage(t, identifier+"-engine", "18")
	created := h.create(t, identifier)
	if err := h.runCreate(t, created.ID); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}
	return h.get(t, created.ID)
}

// runCreate runs the create task the way the worker does: the real task row
// with the payload the service stored, and the bookkeeping the pool does once
// the handler returned. Running it twice is how a requeued task arrives.
func (h *harness) runCreate(t *testing.T, dbInstanceID string) error {
	t.Helper()
	return h.runTask(t, dbInstanceID, h.svc.CreateHandler)
}

// runDelete runs the delete task the same way.
func (h *harness) runDelete(t *testing.T, dbInstanceID string) error {
	t.Helper()
	return h.runTask(t, dbInstanceID, h.svc.DeleteHandler)
}

func (h *harness) runTask(
	t *testing.T, resourceID string, handler func(context.Context, worker.Task) error,
) error {
	t.Helper()

	row, err := h.store.GetLatestInstanceTask(t.Context(), sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: resourceID, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetLatestInstanceTask(%q): %v", resourceID, err)
	}

	err = handler(t.Context(), worker.Task{
		ID:         row.ID,
		Type:       row.Type,
		ResourceID: resourceID,
		Payload:    json.RawMessage(row.Payload),
	})
	h.finishTask(t, row, err)
	return err
}

// finishTask does the bookkeeping the worker pool does after a handler returns,
// so the next action does not refuse because a task is still in flight.
func (h *harness) finishTask(t *testing.T, row sqlc.Task, cause error) {
	t.Helper()

	params := sqlc.UpdateTaskParams{
		ID:        row.ID,
		State:     string(task.StateSucceeded),
		Progress:  100,
		Attempts:  row.Attempts + 1,
		UpdatedAt: h.now.UTC().Format(time.RFC3339Nano),
	}
	if cause != nil {
		params.State = string(task.StateFailed)
		params.Progress = row.Progress
		params.Error = sql.NullString{String: cause.Error(), Valid: true}
	}
	if _, err := h.store.UpdateTask(context.WithoutCancel(t.Context()), params); err != nil {
		t.Fatalf("UpdateTask(%q): %v", row.ID, err)
	}
}

// runInstanceTask runs the task a delegated lifecycle action queued on the VM,
// which is what moves the guest and lets the reconcile pass judge the database.
//
// It claims the pending task rather than the most recent one: the harness clock
// is frozen, so two tasks created in one test tie on created_at and "most
// recent" would be whichever id sorted higher.
func (h *harness) runInstanceTask(t *testing.T, instanceID string) error {
	t.Helper()

	row, err := h.store.GetLatestInstanceTask(t.Context(), sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: instanceID, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetLatestInstanceTask(%q): %v", instanceID, err)
	}
	if task.State(row.State) != task.StatePending {
		var id, taskType, payload string
		if err := h.store.DB().QueryRowContext(t.Context(),
			`SELECT id, type, payload FROM tasks WHERE resource_id = ? AND state = 'pending'
			 ORDER BY created_at DESC, id DESC LIMIT 1`, instanceID).
			Scan(&id, &taskType, &payload); err != nil {
			t.Fatalf("read the pending task of %q: %v", instanceID, err)
		}
		row.ID, row.Type, row.Payload = id, taskType, payload
	}

	handler, ok := h.instances.Handler()[row.Type]
	if !ok {
		t.Fatalf("no handler for %q", row.Type)
	}
	err = handler.Handle(t.Context(), worker.Task{
		ID:         row.ID,
		Type:       row.Type,
		ResourceID: instanceID,
		Payload:    json.RawMessage(row.Payload),
	})
	h.finishTask(t, row, err)
	return err
}

// newHarnessOver builds a second service over the same database, fake and
// instance service: exactly what a restart leaves behind, with every row in
// place and the credential stash empty.
func newHarnessOver(t *testing.T, h harness) harness {
	t.Helper()

	svc, err := dbinstance.New(h.opts)
	if err != nil {
		t.Fatalf("dbinstance.New(): %v", err)
	}
	restarted := h
	restarted.svc = svc
	return restarted
}

// taskFor is the most recent task of a resource, in the shape the worker hands
// a handler.
func (h *harness) taskFor(t *testing.T, resourceID string) worker.Task {
	t.Helper()

	row, err := h.store.GetLatestInstanceTask(t.Context(), sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: resourceID, Valid: true},
	})
	if err != nil {
		t.Fatalf("GetLatestInstanceTask(%q): %v", resourceID, err)
	}
	return worker.Task{
		ID:         row.ID,
		Type:       row.Type,
		ResourceID: resourceID,
		Payload:    json.RawMessage(row.Payload),
	}
}

// taskOfType is the most recent task of one type for a resource, which is how a
// test reaches the create task of a DB instance that has since been asked to
// go away.
func (h *harness) taskOfType(t *testing.T, resourceID, taskType string) worker.Task {
	t.Helper()

	var id, payload string
	if err := h.store.DB().QueryRowContext(t.Context(),
		`SELECT id, payload FROM tasks WHERE resource_id = ? AND type = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		resourceID, taskType).Scan(&id, &payload); err != nil {
		t.Fatalf("read the %s task of %q: %v", taskType, resourceID, err)
	}
	return worker.Task{
		ID:         id,
		Type:       taskType,
		ResourceID: resourceID,
		Payload:    json.RawMessage(payload),
	}
}

// get reads a DB instance back, failing the test if it is gone.
func (h *harness) get(t *testing.T, dbInstanceID string) dbinstance.DBInstance {
	t.Helper()

	got, err := h.svc.Get(t.Context(), dbInstanceID)
	if err != nil {
		t.Fatalf("Get(%q): %v", dbInstanceID, err)
	}
	return got
}

// row reads the db_instances row, so a test asserts on the row rather than on
// the projection the same service built.
func (h *harness) row(t *testing.T, dbInstanceID string) sqlc.DbInstance {
	t.Helper()

	row, err := h.store.GetDBInstance(t.Context(), sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		t.Fatalf("GetDBInstance(%q): %v", dbInstanceID, err)
	}
	return row
}

// instanceState reads the state of the backing VM.
func (h *harness) instanceState(t *testing.T, instanceID string) string {
	t.Helper()

	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		t.Fatalf("GetInstance(%q): %v", instanceID, err)
	}
	return row.State
}

// volumeNames lists the volumes of the instances pool, so a rollback can be
// asserted without sorting anything.
func (h *harness) volumeNames() []string {
	var names []string
	for _, volume := range h.hv.Volumes() {
		if volume.Pool == instancesPool {
			names = append(names, volume.Name)
		}
	}
	return names
}

func (h *harness) hasVolume(name string) bool {
	for _, got := range h.volumeNames() {
		if got == name {
			return true
		}
	}
	return false
}

// advance steps the harness clock forward, which is how a test reaches a
// timestamp comparison the service makes on stored times.
func (h harness) advance(d time.Duration) { *h.now = h.now.Add(d) }

// seedData is the rendered seed the fake recorded for an instance's seed ISO,
// so a test can assert on what the guest would have read.
func (h *harness) seedData(t *testing.T, instanceID string) string {
	t.Helper()

	for _, volume := range h.hv.Volumes() {
		if volume.Pool == instancesPool && volume.Name == hypervisor.SeedISOName(instanceID) {
			return volume.Content
		}
	}
	t.Fatalf("no seed volume for %s", instanceID)
	return ""
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	hv := fake.New(fake.Config{})
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	base := dbinstance.Options{
		Logger:        logging.Discard(),
		Store:         db,
		Hypervisor:    hv,
		Instances:     &instance.Service{},
		Tasks:         tasks,
		InstancesPool: instancesPool,
	}

	tests := []struct {
		name string
		edit func(*dbinstance.Options)
		want error
	}{
		{"no logger", func(o *dbinstance.Options) { o.Logger = nil }, dbinstance.ErrNoLogger},
		{"no store", func(o *dbinstance.Options) { o.Store = nil }, dbinstance.ErrNoStore},
		{"no hypervisor", func(o *dbinstance.Options) { o.Hypervisor = nil }, dbinstance.ErrNoHypervisor},
		{"no instances", func(o *dbinstance.Options) { o.Instances = nil }, dbinstance.ErrNoInstances},
		{"no tasks", func(o *dbinstance.Options) { o.Tasks = nil }, dbinstance.ErrNoTasks},
		{"no pool", func(o *dbinstance.Options) { o.InstancesPool = "" }, dbinstance.ErrNoInstancesPool},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := base
			tc.edit(&opts)
			if _, err := dbinstance.New(opts); !errors.Is(err, tc.want) {
				t.Errorf("New() error = %v, want %v", err, tc.want)
			}
		})
	}

	if _, err := dbinstance.New(base); err != nil {
		t.Errorf("New() with every dependency: %v", err)
	}
}

// A node where nobody has imported an engine image yet is the normal state of a
// fresh install, so the refusal carries its own code: the console explains the
// import rather than reporting a fault (section 2).
func TestCreateWithoutAnEngineImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.svc.Create(t.Context(), h.createInput("orders"))
	if !errors.Is(err, dbinstance.ErrNoEngineImage) {
		t.Fatalf("Create() error = %v, want ErrNoEngineImage", err)
	}
	// It is a conflict as well, so a client that only knows the standard codes
	// still gets a 409 rather than a 500.
	if !errors.Is(err, service.ErrConflict) {
		t.Errorf("Create() error = %v, want it to wrap ErrConflict", err)
	}

	// And nothing was reserved on the way to the refusal.
	page, err := h.instances.List(t.Context(), instance.ListInput{IncludeManaged: true})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("the refused create left %d instances behind", len(page.Items))
	}
}

// An engine image that is still importing cannot boot anything, so it does not
// count as one.
func TestCreateIgnoresAnUnavailableEngineImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := h.images.Import(t.Context(), image.ImportInput{
		Name:          "postgres-18",
		SourceURL:     "https://example.invalid/postgres-18.qcow2",
		MinDiskGB:     engineDiskGB,
		Kind:          image.KindEngine,
		Engine:        image.EnginePostgres,
		EngineVersion: "18",
	}); err != nil {
		t.Fatalf("Import(): %v", err)
	}

	if _, err := h.svc.Create(t.Context(), h.createInput("orders")); !errors.Is(
		err, dbinstance.ErrNoEngineImage) {
		t.Errorf("Create() error = %v, want ErrNoEngineImage", err)
	}
}

// A version nobody imported is a conflict naming the version, not a 500.
func TestCreateWithAnUnknownEngineVersion(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")

	in := h.createInput("orders")
	in.EngineVersion = "17"
	_, err := h.svc.Create(t.Context(), in)
	if !errors.Is(err, dbinstance.ErrNoEngineImage) {
		t.Fatalf("Create() error = %v, want ErrNoEngineImage", err)
	}
	if !strings.Contains(err.Error(), "17") {
		t.Errorf("Create() error = %q, want it to name the version asked for", err)
	}
}

// Omitting the version takes the newest one, which is what the create form does
// when an operator has not chosen.
func TestCreateWithoutAVersionTakesTheNewest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-17", "17")
	h.advance(time.Second)
	h.engineImage(t, "postgres-18", "18")

	in := h.createInput("orders")
	in.EngineVersion = ""
	created, err := h.svc.Create(t.Context(), in)
	if err != nil {
		t.Fatalf("Create(): %v", err)
	}
	if created.EngineVersion != "18" {
		t.Errorf("EngineVersion = %q, want 18", created.EngineVersion)
	}
	// The row records the resolved version, so a DB instance created without
	// naming one still says which it got.
	if row := h.row(t, created.ID); row.EngineVersion != "18" {
		t.Errorf("the row's engine_version = %q, want 18", row.EngineVersion)
	}
}

// The create reserves the VM, the address, the row and the task in one
// transaction, and answers with the DB instance creating.
func TestCreateReservesEverythingAtOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	engine := h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	if created.State != dbinstance.StateCreating {
		t.Errorf("State = %q, want creating", created.State)
	}
	if created.Endpoint != nil {
		t.Errorf("Endpoint = %v, want null before the guest has an address", *created.Endpoint)
	}
	if created.TaskID == nil {
		t.Fatal("TaskID is nil; the console has nothing to follow")
	}
	if created.ImageID != engine.ID {
		t.Errorf("ImageID = %q, want the engine image %q", created.ImageID, engine.ID)
	}
	if created.Port != dbinstance.DefaultPort {
		t.Errorf("Port = %d, want %d", created.Port, dbinstance.DefaultPort)
	}
	if created.VCPUs == 0 || created.MemoryMiB == 0 {
		t.Errorf("VCPUs = %d and MemoryMiB = %d, want the instance type's preset",
			created.VCPUs, created.MemoryMiB)
	}
	if !id.HasResourceShape(id.PrefixDBInstance, created.ID) {
		t.Errorf("ID = %q, want a db-… id", created.ID)
	}

	// The backing VM exists, is pending, is owned by this module and carries
	// neither user data nor a key pair.
	server, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: created.InstanceID})
	if err != nil {
		t.Fatalf("GetInstance(%q): %v", created.InstanceID, err)
	}
	if server.ManagedBy != string(instance.ManagedByDatabases) {
		t.Errorf("managed_by = %q, want %q", server.ManagedBy, instance.ManagedByDatabases)
	}
	if server.Name != "orders" {
		t.Errorf("the VM's name = %q, want the identifier", server.Name)
	}
	if server.UserData.Valid {
		t.Error("the VM carries user_data; the generated seed must not be persisted")
	}
	if server.KeyPairID.Valid {
		t.Error("the VM carries a key pair; a DB instance has no interactive login")
	}
	if !server.PrivateIp.Valid || !server.MacAddress.Valid {
		t.Error("the VM has no address or MAC; the reservation did not allocate one")
	}

	// And it is invisible to every Instances page, which is the whole of R7.
	page, err := h.instances.List(t.Context(), instance.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("the Instances list shows %d rows, want none", len(page.Items))
	}
}

// The data disk counts against the node as much as the root overlay does: a
// create the host cannot hold is refused before anything is written.
func TestCreateRefusesWhatTheNodeCannotHold(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, fake.Config{Capacity: hypervisor.Capacity{
		VCPUs:       4,
		MemoryBytes: 8 << 30,
		DiskBytes:   10 << 30,
	}}, dbinstance.Options{})
	h.engineImage(t, "postgres-18", "18")

	in := h.createInput("orders")
	in.StorageGB = 2000
	_, err := h.svc.Create(t.Context(), in)
	if !errors.Is(err, service.ErrQuota) {
		t.Fatalf("Create() error = %v, want ErrQuota", err)
	}

	page, err := h.instances.List(t.Context(), instance.ListInput{IncludeManaged: true})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("the refused create left %d instances behind", len(page.Items))
	}
}

// One live identifier at a time, and the backing VM shares the live-name index
// with every user instance, so an identifier that collides with an instance
// name is refused too.
func TestCreateRefusesATakenIdentifier(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	h.create(t, "orders")

	_, err := h.svc.Create(t.Context(), h.createInput("orders"))
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Fatalf("Create() error = %v, want ErrAlreadyExists", err)
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Errorf("Create() error = %q, want it to name the identifier", err)
	}
}

// The storage floor of section 3: at least the engine image's min_disk_gb.
func TestCreateRefusesStorageSmallerThanTheImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImageWithMin(t, "postgres-18", "18", 30)

	in := h.createInput("orders")
	in.StorageGB = 20
	_, err := h.svc.Create(t.Context(), in)

	var invalid *service.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("Create() error = %v, want a validation error", err)
	}
	if len(invalid.Fields) != 1 || invalid.Fields[0].Field != "storage_gb" {
		t.Errorf("fields = %+v, want storage_gb", invalid.Fields)
	}
}

// Every field-level refusal, each one naming the field the console marks.
func TestCreateValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")

	tests := []struct {
		name  string
		edit  func(*dbinstance.CreateInput)
		field string
	}{
		{"no identifier", func(in *dbinstance.CreateInput) { in.Identifier = "" }, "identifier"},
		{"identifier in capitals", func(in *dbinstance.CreateInput) { in.Identifier = "Orders" }, "identifier"},
		{"identifier starting with a digit", func(in *dbinstance.CreateInput) { in.Identifier = "1orders" }, "identifier"},
		{"identifier ending in a dash", func(in *dbinstance.CreateInput) { in.Identifier = "orders-" }, "identifier"},
		{"identifier with a doubled dash", func(in *dbinstance.CreateInput) { in.Identifier = "or--ders" }, "identifier"},
		{"identifier too long", func(in *dbinstance.CreateInput) {
			in.Identifier = "a" + strings.Repeat("b", dbinstance.MaxIdentifierLength)
		}, "identifier"},
		{"identifier with an underscore", func(in *dbinstance.CreateInput) { in.Identifier = "or_ders" }, "identifier"},
		{"another engine", func(in *dbinstance.CreateInput) { in.Engine = "mysql" }, "engine"},
		{"a minor version", func(in *dbinstance.CreateInput) { in.EngineVersion = "18.6" }, "engine_version"},
		{"no instance type", func(in *dbinstance.CreateInput) { in.InstanceType = "" }, "instance_type"},
		{"a subnet that is not an id", func(in *dbinstance.CreateInput) { in.SubnetID = "default" }, "subnet_id"},
		{"storage below the floor", func(in *dbinstance.CreateInput) { in.StorageGB = 1 }, "storage_gb"},
		{"storage above the ceiling", func(in *dbinstance.CreateInput) {
			in.StorageGB = dbinstance.MaxStorageGB + 1
		}, "storage_gb"},
		{"no admin username", func(in *dbinstance.CreateInput) { in.AdminUsername = "" }, "admin_username"},
		{"an admin username with a dash", func(in *dbinstance.CreateInput) { in.AdminUsername = "my-app" }, "admin_username"},
		{"postgres as the admin", func(in *dbinstance.CreateInput) { in.AdminUsername = "postgres" }, "admin_username"},
		{"a pg_ prefixed admin", func(in *dbinstance.CreateInput) { in.AdminUsername = "pg_monitor" }, "admin_username"},
		{"rdsadmin as the admin", func(in *dbinstance.CreateInput) { in.AdminUsername = "rdsadmin" }, "admin_username"},
		{"an admin username too long", func(in *dbinstance.CreateInput) {
			in.AdminUsername = "a" + strings.Repeat("b", dbinstance.MaxNameLength)
		}, "admin_username"},
		{"no database name", func(in *dbinstance.CreateInput) { in.DatabaseName = "" }, "database_name"},
		{"template1 as the database", func(in *dbinstance.CreateInput) { in.DatabaseName = "template1" }, "database_name"},
		{"a database name in capitals", func(in *dbinstance.CreateInput) { in.DatabaseName = "Orders" }, "database_name"},
		{"no password", func(in *dbinstance.CreateInput) { in.AdminPassword = "" }, "admin_password"},
		{"a short password", func(in *dbinstance.CreateInput) { in.AdminPassword = "short" }, "admin_password"},
		{"a password with a long dash", func(in *dbinstance.CreateInput) {
			in.AdminPassword = "passphrase—with-a-dash"
		}, "admin_password"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := h.createInput("orders")
			tc.edit(&in)

			_, err := h.svc.Create(t.Context(), in)
			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Create() error = %v, want a validation error", err)
			}
			if !hasField(invalid.Fields, tc.field) {
				t.Errorf("fields = %+v, want one naming %q", invalid.Fields, tc.field)
			}
		})
	}
}

// A refused password must not be echoed back in the message a form renders.
func TestCreateNeverEchoesThePassword(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")

	in := h.createInput("orders")
	in.AdminPassword = "shørt"
	_, err := h.svc.Create(t.Context(), in)
	if err == nil {
		t.Fatal("Create() accepted a password that is not printable ASCII")
	}
	if strings.Contains(err.Error(), in.AdminPassword) {
		t.Errorf("Create() error = %q, want it not to echo the password", err)
	}

	var invalid *service.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("Create() error = %v, want a validation error", err)
	}
	for _, field := range invalid.Fields {
		if strings.Contains(field.Reason, in.AdminPassword) {
			t.Errorf("the reason for %s echoes the password", field.Field)
		}
	}
}

func hasField(fields []service.FieldError, name string) bool {
	for _, field := range fields {
		if field.Field == name {
			return true
		}
	}
	return false
}

// A missing DB instance is a 404 rather than an empty projection.
func TestGetUnknownDBInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := h.svc.Get(t.Context(), "db-0a1b2c3d4e5f60718"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestListPagesSortsAndSearches(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")

	var ids []string
	for _, identifier := range []string{"orders", "billing", "reports"} {
		ids = append(ids, h.create(t, identifier).ID)
		h.advance(time.Second)
	}

	page, err := h.svc.List(t.Context(), dbinstance.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("List() returned %d items, want 3", len(page.Items))
	}
	// Newest first by default.
	if page.Items[0].ID != ids[2] {
		t.Errorf("List()[0] = %q, want the newest %q", page.Items[0].ID, ids[2])
	}

	asc, err := h.svc.List(t.Context(), dbinstance.ListInput{Sort: dbinstance.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(asc): %v", err)
	}
	if asc.Items[0].ID != ids[0] {
		t.Errorf("List(asc)[0] = %q, want the oldest %q", asc.Items[0].ID, ids[0])
	}

	found, err := h.svc.List(t.Context(), dbinstance.ListInput{Query: "BILL"})
	if err != nil {
		t.Fatalf("List(q): %v", err)
	}
	if len(found.Items) != 1 || found.Items[0].Identifier != "billing" {
		t.Errorf("List(q=BILL) = %+v, want only billing", found.Items)
	}

	filtered, err := h.svc.List(t.Context(), dbinstance.ListInput{State: dbinstance.StateAvailable})
	if err != nil {
		t.Fatalf("List(state): %v", err)
	}
	if len(filtered.Items) != 0 {
		t.Errorf("List(state=available) = %d items, want none while all three are creating",
			len(filtered.Items))
	}

	first, err := h.svc.List(t.Context(), dbinstance.ListInput{PageSize: 2})
	if err != nil {
		t.Fatalf("List(page_size=2): %v", err)
	}
	if len(first.Items) != 2 || first.NextPageToken == nil {
		t.Fatalf("List(page_size=2) = %d items, token %v", len(first.Items), first.NextPageToken)
	}
	second, err := h.svc.List(t.Context(), dbinstance.ListInput{
		PageSize: 2, PageToken: *first.NextPageToken,
	})
	if err != nil {
		t.Fatalf("List(page 2): %v", err)
	}
	if len(second.Items) != 1 || second.NextPageToken != nil {
		t.Errorf("List(page 2) = %d items, token %v", len(second.Items), second.NextPageToken)
	}
}

func TestListRefusesWhatItCannotAnswer(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name  string
		in    dbinstance.ListInput
		field string
	}{
		{"an unknown sort", dbinstance.ListInput{Sort: "identifier:asc"}, "sort"},
		{"an unknown state", dbinstance.ListInput{State: "terminated"}, "state"},
		{"a page size of zero is a default, not an error", dbinstance.ListInput{PageSize: -1}, "page_size"},
		{"a page size above the ceiling", dbinstance.ListInput{PageSize: dbinstance.MaxPageSize + 1}, "page_size"},
		{"a token this API did not issue", dbinstance.ListInput{PageToken: "not-a-token"}, "page_token"},
		{"a token naming something that is not a DB instance", dbinstance.ListInput{
			PageToken: encodeToken("2026-05-06T10:00:00Z", "i-0a1b2c3d4e5f60718"),
		}, "page_token"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := h.svc.List(t.Context(), tc.in)
			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("List() error = %v, want a validation error", err)
			}
			if !hasField(invalid.Fields, tc.field) {
				t.Errorf("fields = %+v, want one naming %q", invalid.Fields, tc.field)
			}
		})
	}
}

// encodeToken builds a page token the way the service does, so a test can offer
// one that is well formed and still wrong.
func encodeToken(createdAt, resourceID string) string {
	return base64RawURL(createdAt + "|" + resourceID)
}

func base64RawURL(value string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

	var out strings.Builder
	data := []byte(value)
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		bits := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		for j := 0; j < n+1; j++ {
			out.WriteByte(alphabet[(bits>>uint(18-6*j))&0x3f])
		}
	}
	return out.String()
}

// A projection never carries a credential, because there is none to carry.
func TestProjectionCarriesNoCredential(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("Marshal(): %v", err)
	}
	body := string(encoded)
	for _, forbidden := range []string{password, "verifier", "salt", "SCRAM", "password"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the DB instance JSON carries %q: %s", forbidden, body)
		}
	}
	// The fields the console does need are there.
	for _, want := range []string{`"admin_username":"app"`, `"database_name":"orders"`,
		`"port":5432`, fmt.Sprintf(`"instance_id":%q`, created.InstanceID)} {
		if !strings.Contains(body, want) {
			t.Errorf("the DB instance JSON is missing %s: %s", want, body)
		}
	}
}
