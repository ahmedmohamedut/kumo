package dbinstance_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/service/dbcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// The whole create, end to end against the fake: both disks, the seed, the
// domain, the probe, and the endpoint published only once the engine answered.
func TestCreateWorkflow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	if err := h.runCreate(t, created.ID); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}

	got := h.get(t, created.ID)
	if got.State != dbinstance.StateAvailable {
		t.Fatalf("State = %q (%v), want available", got.State, got.StateReason)
	}
	if got.Endpoint == nil {
		t.Fatal("Endpoint is nil; a DB instance nobody can resolve is not available")
	}
	// The identifier under the VPC's name, which is the DNS domain dnsmasq
	// serves for that network (R9).
	if *got.Endpoint != "orders.default" {
		t.Errorf("Endpoint = %q, want orders.default", *got.Endpoint)
	}
	if state := h.instanceState(t, got.InstanceID); state != string(instance.StateRunning) {
		t.Errorf("the VM is %q, want running", state)
	}

	// Three volumes: the root overlay, the seed and the data disk, the last one
	// named after the DB instance so it survives a replaced root disk.
	for _, want := range []string{
		hypervisor.RootDiskName(got.InstanceID),
		hypervisor.SeedISOName(got.InstanceID),
		hypervisor.DataDiskName(got.ID),
	} {
		if !h.hasVolume(want) {
			t.Errorf("the pool is missing %q; it holds %v", want, h.volumeNames())
		}
	}

	// The data disk is blank and the size that was asked for: a database is
	// initialized by initdb, so there is nothing to clone in.
	for _, volume := range h.hv.Volumes() {
		if volume.Name != hypervisor.DataDiskName(got.ID) {
			continue
		}
		if volume.Backing != "" {
			t.Errorf("the data disk has a backing file %q, want none", volume.Backing)
		}
		if volume.VirtualSizeBytes != int64(got.StorageGB)<<30 {
			t.Errorf("the data disk is %d bytes, want %d GiB",
				volume.VirtualSizeBytes, got.StorageGB)
		}
	}

	// The probe dialed the guest's own address on the DB instance's port.
	if h.probe.count() == 0 {
		t.Error("the create never probed the engine")
	}
	if want := "192.168.100.2:5432"; h.probe.lastAddress() != want {
		t.Errorf("the probe dialed %q, want %q", h.probe.lastAddress(), want)
	}
}

// The seed carries the verifier and the first-boot script, and not the
// password: section 5's promise is a property of what is on the ISO.
func TestCreateSeedCarriesTheVerifierAndNotThePassword(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")
	if err := h.runCreate(t, created.ID); err != nil {
		t.Fatalf("CreateHandler(): %v", err)
	}

	seed := h.seedData(t, created.InstanceID)
	if strings.Contains(seed, password) {
		t.Error("the seed carries the admin password")
	}
	if !strings.Contains(seed, "SCRAM-SHA-256$4096:") {
		t.Error("the seed carries no SCRAM verifier; the role would have no credential")
	}
	// The pieces the first boot cannot work without, from the script of
	// section 5.
	for _, want := range []string{
		dbcloudinit.DataDiskDevice,
		"LABEL=" + dbcloudinit.DataDiskLabel,
		"CLUSTER=18",
		"pg_createcluster \"$CLUSTER\" main",
		"CREATE ROLE",
		"CREATE DATABASE",
		"scram-sha-256",
		"192.168.100.0/24",
		"ssh_pwauth: false",
	} {
		if !strings.Contains(seed, want) {
			t.Errorf("the seed is missing %q", want)
		}
	}
	// And the one thing a DB instance must never offer.
	if strings.Contains(seed, "0.0.0.0/0") {
		t.Error("the seed admits every address; a DB instance is VPC-internal (R6)")
	}
}

// Rollback: a failure at each step of the build leaves no volume, no domain, no
// DHCP host and a DB instance that says why.
func TestCreateRollsBackEveryStep(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		op   hypervisor.Op
	}{
		{"the data disk", hypervisor.OpCreateDataDisk},
		{"the root overlay", hypervisor.OpCreateRootDisk},
		{"the seed", hypervisor.OpCreateSeedISO},
		{"the dhcp host", hypervisor.OpAddDHCPHost},
		{"the domain", hypervisor.OpDefineAndStart},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			h.engineImage(t, "postgres-18", "18")
			created := h.create(t, "orders")

			h.hv.FailAlways(tc.op, errBackendSaidNo)
			if err := h.runCreate(t, created.ID); err == nil {
				t.Fatal("CreateHandler() succeeded with the backend refusing " + string(tc.op))
			}

			got := h.get(t, created.ID)
			if got.State != dbinstance.StateFailed {
				t.Errorf("State = %q, want failed", got.State)
			}
			if got.StateReason == nil || *got.StateReason == "" {
				t.Error("a failed DB instance with no reason is one nobody can act on")
			}
			if got.Endpoint != nil {
				t.Errorf("Endpoint = %q on a failed create", *got.Endpoint)
			}

			// Nothing left in the pool, for any of the three volumes.
			if names := h.volumeNames(); len(names) != 0 {
				t.Errorf("the rollback left %v in the pool", names)
			}
			// No domain and no DHCP host.
			if _, err := h.hv.GetDomain(t.Context(), created.InstanceID); !errors.Is(
				err, hypervisor.ErrNotFound) {
				t.Errorf("GetDomain() error = %v, want the domain to be gone", err)
			}
			network, err := h.hv.GetNetwork(t.Context(), h.vpcID)
			if err != nil {
				t.Fatalf("GetNetwork(): %v", err)
			}
			if len(network.DHCPHosts) != 0 {
				t.Errorf("the rollback left %d dhcp hosts behind", len(network.DHCPHosts))
			}

			// The rows stay: a failed create is read, and delete is the way out.
			if _, err := h.store.GetInstance(t.Context(),
				sqlc.GetInstanceParams{ID: created.InstanceID}); err != nil {
				t.Errorf("the backing row is gone: %v", err)
			}
		})
	}
}

// A requeued task adopts its own data disk, seed and domain rather than failing
// on them -- and it must not need the credential a second time, because the
// seed that already exists is the one carrying it.
func TestCreateRequeueAdoptsWhatItMade(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	// First attempt: everything is built and the engine never answers, which
	// is a first boot that is still working.
	h.probe.set(false)
	if err := h.runCreate(t, created.ID); err == nil {
		t.Fatal("CreateHandler() succeeded while the engine was silent")
	}
	volumes := len(h.volumeNames())
	if volumes != 3 {
		t.Fatalf("the pool holds %d volumes after the first attempt, want 3: %v",
			volumes, h.volumeNames())
	}

	// The row is failed, so put it back where a requeue finds it. This is what
	// worker.Recover does to the task, not to the row; the state is reset here
	// because only the task is requeued in production and a failed create is
	// not retried on its own.
	if _, err := h.store.SetDBInstanceState(t.Context(), sqlc.SetDBInstanceStateParams{
		ID:        created.ID,
		State:     string(dbinstance.StateCreating),
		UpdatedAt: h.now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("SetDBInstanceState(): %v", err)
	}

	// Second attempt: the engine answers, and nothing is created twice.
	h.probe.set(true)
	if err := h.runCreate(t, created.ID); err != nil {
		t.Fatalf("the requeued CreateHandler(): %v", err)
	}
	if got := len(h.volumeNames()); got != volumes {
		t.Errorf("the requeued attempt left %d volumes, want %d: %v",
			got, volumes, h.volumeNames())
	}
	if got := h.get(t, created.ID); got.State != dbinstance.StateAvailable {
		t.Errorf("State = %q, want available", got.State)
	}
}

// A create that has already finished is a no-op, so a task the worker requeues
// after the fact cannot build a second domain.
func TestCreateIsANoOpOnceAvailable(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")
	volumes := h.volumeNames()

	if err := h.runCreate(t, created.ID); err != nil {
		t.Fatalf("the repeated CreateHandler(): %v", err)
	}
	if got := h.volumeNames(); len(got) != len(volumes) {
		t.Errorf("the repeated attempt left %v, want %v", got, volumes)
	}
}

// A guest that runs with an engine that never answers is not rolled back. The
// VM is fine, and throwing it away destroys the evidence the operator needs.
func TestCreateLeavesTheGuestAloneWhenTheEngineIsSilent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	h.probe.set(false)
	err := h.runCreate(t, created.ID)
	if !errors.Is(err, dbinstance.ErrEngineNotReady) {
		t.Fatalf("CreateHandler() error = %v, want ErrEngineNotReady", err)
	}

	got := h.get(t, created.ID)
	if got.State != dbinstance.StateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.StateReason == nil || !strings.Contains(*got.StateReason, "did not answer") {
		t.Errorf("StateReason = %v, want it to say the engine did not answer", got.StateReason)
	}

	// The guest is still defined and running, and every volume is still there.
	info, err := h.hv.GetDomain(t.Context(), created.InstanceID)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	if info.State != hypervisor.DomainStateRunning {
		t.Errorf("the domain is %q, want it left running", info.State)
	}
	if names := h.volumeNames(); len(names) != 3 {
		t.Errorf("the pool holds %v, want all three volumes left in place", names)
	}
	// The console log is where the operator looks next, so it has to exist.
	if _, err := os.Stat(filepath.Join(h.console, created.InstanceID+".log")); err != nil {
		t.Errorf("the console log is missing: %v", err)
	}
}

// A create whose process restarted cannot render the seed again: the password
// was discarded when the request was answered, and nothing persists the
// verifier. It says so instead of booting a database nobody can log in to.
func TestCreateWithoutItsCredential(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	// A fresh service over the same database is exactly what a restart leaves:
	// the rows are there and the stash is empty.
	restarted := newHarnessOver(t, h)
	err := restarted.runCreate(t, created.ID)
	if !errors.Is(err, dbinstance.ErrCredentialGone) {
		t.Fatalf("CreateHandler() error = %v, want ErrCredentialGone", err)
	}

	got := restarted.get(t, created.ID)
	if got.State != dbinstance.StateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.StateReason == nil || !strings.Contains(*got.StateReason, "create it again") {
		t.Errorf("StateReason = %v, want it to tell the operator what to do", got.StateReason)
	}
	// And the rollback cleaned up: there is nothing half-built to adopt.
	if names := restarted.volumeNames(); len(names) != 0 {
		t.Errorf("the failed create left %v behind", names)
	}
}

// Delete removes the guest, the data disk, both rows and the console log, and
// is safe to run twice.
func TestDeleteWorkflow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")

	if _, err := h.svc.Delete(t.Context(), created.ID); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if got := h.row(t, created.ID); got.State != string(dbinstance.StateDeleting) {
		t.Errorf("State = %q, want deleting", got.State)
	}

	if err := h.runDelete(t, created.ID); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}

	if names := h.volumeNames(); len(names) != 0 {
		t.Errorf("the delete left %v in the pool", names)
	}
	if _, err := h.hv.GetDomain(t.Context(), created.InstanceID); !errors.Is(
		err, hypervisor.ErrNotFound) {
		t.Errorf("GetDomain() error = %v, want the domain to be gone", err)
	}
	network, err := h.hv.GetNetwork(t.Context(), h.vpcID)
	if err != nil {
		t.Fatalf("GetNetwork(): %v", err)
	}
	if len(network.DHCPHosts) != 0 {
		t.Errorf("the delete left %d dhcp hosts behind", len(network.DHCPHosts))
	}

	// Both rows are gone, and the interface went with the instance through the
	// cascade.
	if _, err := h.store.GetDBInstance(t.Context(),
		sqlc.GetDBInstanceParams{ID: created.ID}); err == nil {
		t.Error("the db_instances row survived the delete")
	}
	if _, err := h.store.GetInstance(t.Context(),
		sqlc.GetInstanceParams{ID: created.InstanceID}); err == nil {
		t.Error("the backing instance row survived the delete")
	}
	interfaces, err := h.store.ListInterfacesByInstances(t.Context(),
		sqlc.ListInterfacesByInstancesParams{InstanceIds: []string{created.InstanceID}})
	if err != nil {
		t.Fatalf("ListInterfacesByInstances(): %v", err)
	}
	if len(interfaces) != 0 {
		t.Errorf("the delete left %d interfaces behind", len(interfaces))
	}
	if _, err := os.Stat(filepath.Join(h.console, created.InstanceID+".log")); !os.IsNotExist(err) {
		t.Errorf("the console log survived the delete: %v", err)
	}

	// Idempotent: a requeued delete finds everything gone and says nothing.
	if err := h.svc.DeleteHandler(t.Context(), h.taskFor(t, created.ID)); err != nil {
		t.Errorf("the repeated DeleteHandler(): %v", err)
	}
}

// Delete is the way out of a create that is stuck, so it must not wait for the
// readiness timeout to run out: the create notices and stands down.
func TestDeleteDuringACreateThatIsWaiting(t *testing.T) {
	t.Parallel()

	// A long readiness timeout: if the create had to wait it out, this test
	// would take that long.
	h := newHarnessWith(t, fake.Config{}, dbinstance.Options{
		ProbeInterval: time.Millisecond,
		ReadyTimeout:  time.Hour,
	})
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")
	h.probe.set(false)

	// The create is running and waiting for an engine that never answers, the
	// way a first boot that went wrong looks.
	createTask := h.taskOfType(t, created.ID, task.TypeDBInstanceCreate)
	done := make(chan error, 1)
	go func() { done <- h.svc.CreateHandler(t.Context(), createTask) }()
	waitFor(t, func() bool { return h.probe.count() > 0 })

	// The delete only writes the row and the task, and deliberately takes no
	// lock, which is what makes it reachable while the create holds the DB
	// instance.
	if _, err := h.svc.Delete(t.Context(), created.ID); err != nil {
		t.Fatalf("Delete(): %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, dbinstance.ErrCreateCancelled) {
			t.Fatalf("CreateHandler() error = %v, want ErrCreateCancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("CreateHandler() waited out the readiness timeout instead of noticing the delete")
	}

	// The create recorded the refusal, and the delete then takes over.
	if got := h.row(t, created.ID); got.State != string(dbinstance.StateDeleting) {
		t.Errorf("State = %q, want the delete to still own the row", got.State)
	}

	// And the delete then finishes normally.
	if err := h.runDelete(t, created.ID); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}
	if names := h.volumeNames(); len(names) != 0 {
		t.Errorf("the delete left %v in the pool", names)
	}
}

// waitFor polls a condition for a second, which is how a test waits on a
// goroutine without a channel the production code does not have.
func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the condition never came true")
}

// A create for a DB instance somebody deleted outright is a no-op rather than a
// failure.
func TestCreateSkipsADeletedDBInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	if _, err := h.store.DeleteDBInstance(t.Context(),
		sqlc.DeleteDBInstanceParams{ID: created.ID}); err != nil {
		t.Fatalf("DeleteDBInstance(): %v", err)
	}
	if err := h.svc.CreateHandler(t.Context(), h.taskFor(t, created.ID)); err != nil {
		t.Errorf("CreateHandler() error = %v, want it to skip a DB instance that is gone", err)
	}
	if names := h.volumeNames(); len(names) != 0 {
		t.Errorf("the skipped create built %v", names)
	}
}

// A task that names nothing is a payload failure, not a panic.
func TestTaskPayloadIsValidated(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for _, handler := range []struct {
		name string
		run  func(task worker.Task) error
	}{
		{"create", func(task worker.Task) error { return h.svc.CreateHandler(t.Context(), task) }},
		{"delete", func(task worker.Task) error { return h.svc.DeleteHandler(t.Context(), task) }},
	} {
		if err := handler.run(worker.Task{
			ID:      "task-0a1b2c3d4e5f60718",
			Payload: []byte(`{}`),
		}); !errors.Is(err, dbinstance.ErrBadTaskPayload) {
			t.Errorf("%s handler error = %v, want ErrBadTaskPayload", handler.name, err)
		}
	}
}
