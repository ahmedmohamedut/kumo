package instance_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// adoptAsManaged turns an instance the harness launched into the VM behind a DB
// instance, which is the shape M7.5 will create directly. It writes the rows by
// hand because the service that owns them does not exist yet.
func adoptAsManaged(t *testing.T, h harness, instanceID, dbInstanceID, identifier string) {
	t.Helper()

	ctx := t.Context()
	if _, err := h.store.DB().ExecContext(ctx,
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByDatabases), instanceID); err != nil {
		t.Fatalf("mark %s managed: %v", instanceID, err)
	}

	row, err := h.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		t.Fatalf("GetInstance(%q): %v", instanceID, err)
	}
	if _, err := h.store.DB().ExecContext(ctx,
		`INSERT INTO db_instances (id, identifier, state, engine, engine_version, image_id,
		   instance_id, instance_type, storage_gb, admin_username, database_name,
		   created_at, updated_at)
		 VALUES (?, ?, 'available', 'postgres', '18', ?, ?, ?, 20, 'app', 'orders', ?, ?)`,
		dbInstanceID, identifier, row.ImageID, instanceID, row.InstanceType,
		row.LaunchedAt, row.UpdatedAt); err != nil {
		t.Fatalf("insert db_instances: %v", err)
	}
}

// launchRunning launches an instance and leaves it running, which is the state
// most lifecycle refusals have to be asserted from.
func launchRunning(t *testing.T, h harness, name string) string {
	t.Helper()

	source := h.availableImage(t, name+"-image")
	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name:         name,
		ImageID:      source.ID,
		InstanceType: instanceType,
		RootDiskGB:   imageDiskGB,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}
	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(): %v", err)
	}
	return launched.ID
}

// The whole point of R7: the VM is not on the Instances pages, however the page
// is sorted, filtered or searched.
func TestListHidesManagedInstances(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	mine := launchRunning(t, h, "web-1")
	h.advance()
	managed := launchRunning(t, h, "orders")
	adoptAsManaged(t, h, managed, "db-0a1b2c3d4e5f60718", "orders")

	for _, in := range []instance.ListInput{
		{},
		{Sort: instance.SortLaunchedAsc},
		{State: instance.StateRunning},
		{Query: "orders"},
	} {
		page, err := h.svc.List(t.Context(), in)
		if err != nil {
			t.Fatalf("List(%+v): %v", in, err)
		}
		for _, got := range page.Items {
			if got.ID == managed {
				t.Errorf("List(%+v) returned the managed instance %s", in, managed)
			}
		}
	}

	page, err := h.svc.List(t.Context(), instance.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != mine {
		t.Errorf("default list returned %d items, want only %s", len(page.Items), mine)
	}

	// And reachable when a caller asks on purpose, which is what keeps the
	// filter a decision rather than a wall.
	page, err = h.svc.List(t.Context(), instance.ListInput{IncludeManaged: true})
	if err != nil {
		t.Fatalf("List(IncludeManaged): %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("IncludeManaged list returned %d items, want 2", len(page.Items))
	}
}

// Hidden from the list, counted by capacity. A database holds real vCPUs,
// memory and disk, and a report that hid them would let the node promise
// capacity it had already handed out.
func TestManagedInstancesStillCountAgainstCapacity(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	before, err := h.store.AllocatedCapacity(t.Context())
	if err != nil {
		t.Fatalf("AllocatedCapacity(): %v", err)
	}

	managed := launchRunning(t, h, "orders")
	adoptAsManaged(t, h, managed, "db-0a1b2c3d4e5f60718", "orders")

	after, err := h.store.AllocatedCapacity(t.Context())
	if err != nil {
		t.Fatalf("AllocatedCapacity(): %v", err)
	}
	if after.VCPUs <= before.VCPUs {
		t.Errorf("vcpus = %d, want more than %d: a managed VM holds real capacity",
			after.VCPUs, before.VCPUs)
	}
	if after.MemoryBytes <= before.MemoryBytes {
		t.Errorf("memory = %d, want more than %d", after.MemoryBytes, before.MemoryBytes)
	}
	if after.DiskBytes <= before.DiskBytes {
		t.Errorf("disk = %d, want more than %d", after.DiskBytes, before.DiskBytes)
	}
}

// The reconciler is libvirt-versus-SQLite and does not care who created a
// domain; a managed VM that drifted is the one nobody is watching by hand.
func TestReconcileStillSeesManagedInstances(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	managed := launchRunning(t, h, "orders")
	adoptAsManaged(t, h, managed, "db-0a1b2c3d4e5f60718", "orders")

	rows, err := h.store.ListInstancesForReconcile(t.Context())
	if err != nil {
		t.Fatalf("ListInstancesForReconcile(): %v", err)
	}
	found := false
	for _, row := range rows {
		if row.ID == managed {
			found = true
		}
	}
	if !found {
		t.Errorf("the reconcile pass skipped the managed instance %s", managed)
	}

	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
}

// Stopping the VM under a running database, or terminating it, would take the
// database down without its own service knowing anything happened.
func TestLifecycleRefusesAManagedInstance(t *testing.T) {
	t.Parallel()

	const (
		dbID       = "db-0a1b2c3d4e5f60718"
		identifier = "orders"
	)

	tests := []struct {
		name string
		call func(context.Context, *instance.Service, string) error
	}{
		{"stop", func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Stop(ctx, id, false)
			return err
		}},
		{"forced stop", func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Stop(ctx, id, true)
			return err
		}},
		{"start", func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Start(ctx, id)
			return err
		}},
		{"reboot", func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Reboot(ctx, id, false)
			return err
		}},
		{"terminate", func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Terminate(ctx, id)
			return err
		}},
		{"rename", func(ctx context.Context, svc *instance.Service, id string) error {
			name := "renamed"
			_, err := svc.Update(ctx, id, instance.UpdateInput{Name: &name}, false)
			return err
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			managed := launchRunning(t, h, "orders")
			adoptAsManaged(t, h, managed, dbID, identifier)

			err := tc.call(t.Context(), h.svc, managed)
			if err == nil {
				t.Fatal("the action was accepted on a managed instance")
			}
			if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("error = %v, want a conflict", err)
			}
			// The refusal has to say where to go instead, or an operator is
			// left guessing which resource owns the VM.
			for _, want := range []string{dbID, identifier, "/api/v1/rds/db-instances"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// The refusal stands even when the owning row cannot be read: being unable to
// name the owner is no reason to let the action through.
func TestLifecycleRefusesAManagedInstanceWithNoOwnerRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	managed := launchRunning(t, h, "orders")
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByDatabases), managed); err != nil {
		t.Fatalf("mark managed: %v", err)
	}

	_, err := h.svc.Terminate(t.Context(), managed)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("error = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "/api/v1/rds/db-instances") {
		t.Errorf("error = %q, want it to point at the databases API", err)
	}
}

// An unmanaged instance is unaffected by all of the above.
func TestLifecycleStillWorksOnAnOrdinaryInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	mine := launchRunning(t, h, "web-1")

	if _, err := h.svc.Stop(t.Context(), mine, false); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
}

// The column is written, not defaulted: a user launch must be unowned even if
// the column default ever changes.
func TestLaunchRecordsNoOwner(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	mine := launchRunning(t, h, "web-1")

	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: mine})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if instance.ManagedBy(row.ManagedBy) != instance.ManagedByNone {
		t.Errorf("managed_by = %q, want empty on a user launch", row.ManagedBy)
	}
}

// The data disk is blank on purpose: a database is initialized by initdb on an
// empty filesystem, so it has no backing file to clone from.
func TestCreateDataDiskIsBlankAndNamedForTheDBInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	const dbID = "db-0a1b2c3d4e5f60718"

	if err := h.hv.CreateDataDisk(t.Context(), hypervisor.DataDiskSpec{
		DBInstanceID: dbID,
		Pool:         instancesPool,
		SizeGiB:      20,
	}); err != nil {
		t.Fatalf("CreateDataDisk(): %v", err)
	}

	want := hypervisor.DataDiskName(dbID)
	if want != dbID+"-data.qcow2" {
		t.Errorf("DataDiskName(%q) = %q, want the db- prefixed name", dbID, want)
	}

	var found bool
	for _, vol := range h.hv.Volumes() {
		if vol.Pool != instancesPool || vol.Name != want {
			continue
		}
		found = true
		if vol.Backing != "" {
			t.Errorf("backing = %q, want none: nothing is cloned into a data disk", vol.Backing)
		}
		if vol.VirtualSizeBytes != 20<<30 {
			t.Errorf("virtual size = %d, want 20 GiB", vol.VirtualSizeBytes)
		}
	}
	if !found {
		t.Errorf("volumes = %v, want %q", h.volumeNames(instancesPool), want)
	}

	// Twice is the requeued-task case, and the data on the disk is the thing a
	// retry must not destroy.
	err := h.hv.CreateDataDisk(t.Context(), hypervisor.DataDiskSpec{
		DBInstanceID: dbID,
		Pool:         instancesPool,
		SizeGiB:      20,
	})
	if !errors.Is(err, hypervisor.ErrAlreadyExists) {
		t.Errorf("second CreateDataDisk() = %v, want ErrAlreadyExists", err)
	}
}

func TestCreateDataDiskValidatesItsSpec(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	tests := []struct {
		name string
		spec hypervisor.DataDiskSpec
	}{
		{"no db instance id", hypervisor.DataDiskSpec{Pool: instancesPool, SizeGiB: 20}},
		{"no pool", hypervisor.DataDiskSpec{DBInstanceID: "db-1", SizeGiB: 20}},
		{"no size", hypervisor.DataDiskSpec{DBInstanceID: "db-1", Pool: instancesPool}},
		{"negative size", hypervisor.DataDiskSpec{DBInstanceID: "db-1", Pool: instancesPool, SizeGiB: -1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := h.hv.CreateDataDisk(t.Context(), tc.spec); err == nil {
				t.Error("CreateDataDisk() accepted a spec it should refuse")
			}
		})
	}
}
