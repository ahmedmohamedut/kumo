package dbinstance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/cloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/dbcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// CreateHandler runs the db.create task: it makes the data disk, builds the VM
// through the instance service, waits for the engine to answer and publishes
// the endpoint (docs/design/07-databases.md section 4).
//
// Two failures are not VM failures and are handled differently from the rest.
// A guest that never takes an address, or a disk that cannot be created, is
// rolled back: nothing works and nothing is worth keeping. A guest that runs
// while the engine never answers is left exactly as it is -- the VM is fine,
// throwing it away destroys the evidence, and the serial log under
// $KUMO_DATA_DIR/console is where the operator looks next.
func (s *Service) CreateHandler(ctx context.Context, t worker.Task) error {
	dbInstanceID, err := decodeCreate(t)
	if err != nil {
		return err
	}

	release, err := s.locks.Acquire(ctx, LockKey(dbInstanceID), t.Type)
	if err != nil {
		return fmt.Errorf("take the lock on DB instance %q: %w", dbInstanceID, err)
	}
	defer release()

	row, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Deleted before the task ran. Nothing to build and nothing to
			// report: the delete took everything with it.
			s.logger.InfoContext(ctx, "dbinstance: create skipped, the DB instance is gone",
				"db_instance_id", dbInstanceID, "task_id", t.ID)
			s.credentials.forget(dbInstanceID)
			return nil
		}
		return fmt.Errorf("get DB instance %q: %w", dbInstanceID, err)
	}

	switch State(row.State) {
	case StateAvailable:
		// A create that already finished. Without this a requeued task would
		// build a second domain for the same DB instance.
		s.logger.InfoContext(ctx, "dbinstance: create already finished",
			"db_instance_id", row.ID, "task_id", t.ID)
		s.credentials.forget(row.ID)
		return nil
	case StateDeleting:
		// A delete is on its way. It is allowed from every state, including
		// this one, so the create stands down rather than rebuilding what the
		// delete is removing.
		s.logger.InfoContext(ctx, "dbinstance: create skipped, the DB instance is being deleted",
			"db_instance_id", row.ID, "task_id", t.ID)
		return nil
	}

	if err := s.create(ctx, row, t); err != nil {
		if ctx.Err() != nil {
			// A shutdown rather than a failure: the task is requeued and the
			// next run adopts the disk, the seed and the domain. Marking the
			// row failed would report a restart as something to act on.
			s.logger.WarnContext(ctx, "dbinstance: create interrupted by shutdown",
				"db_instance_id", row.ID, "task_id", t.ID, "error", err)
			return err
		}
		if errors.Is(err, ErrCreateCancelled) {
			// The delete owns the row now. Writing `failed` over `deleting`
			// would show a failure for a database that is simply going away,
			// and the delete removes what this run built either way.
			s.credentials.forget(row.ID)
			return err
		}
		s.failCreate(ctx, row, t.ID, err)
		return err
	}

	s.credentials.forget(row.ID)
	return nil
}

// create runs the steps of the create workflow.
func (s *Service) create(ctx context.Context, row sqlc.DbInstance, t worker.Task) error {
	placement, err := s.placement(ctx, row)
	if err != nil {
		return err
	}

	// Step 1: the data disk, which carries no backing file. A database is
	// initialized by initdb on an empty filesystem, so there is nothing to
	// clone in (section 4 step 1).
	dataDisk := hypervisor.DataDiskName(row.ID)
	created, err := s.createDataDisk(ctx, row, dataDisk)
	if err != nil {
		return err
	}

	// Steps 2 to 5: the root overlay, the seed, the static DHCP host and the
	// domain, with the address the guest must come up with. The instance
	// service owns them, including the rollback of whatever it managed to
	// create, so there is one implementation of a launch and not two.
	launched, err := s.instances.LaunchManaged(ctx, row.InstanceID, instance.ManagedLaunch{
		DataDiskName: dataDisk,
		Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
			return s.seed(ctx, row, placement)
		},
	})
	if err != nil {
		if ctx.Err() == nil && created {
			// The data disk is this package's to remove. It is only deleted
			// when this run created it: a requeued task that adopted the disk
			// of an earlier attempt must not throw away an initialized
			// cluster.
			s.deleteDataDisk(ctx, row.ID, dataDisk)
		}
		return err
	}

	// Step 6: the engine. The probe means "something is listening on the VPC
	// address", and because the first-boot script opens that address last,
	// that already implies the role and the database exist (section 6).
	address := net.JoinHostPort(launched.PrivateIP, strconv.Itoa(int(row.Port)))
	if err := s.awaitEngine(ctx, row.ID, address); err != nil {
		// The guest is demonstrably up -- LaunchManaged waited for its address
		// -- so the VM row is brought in line even though the create failed.
		// Without it the row would stay `pending`, which both reconcile passes
		// skip as "owned by a launch", and an engine that came up a minute
		// later could never move the DB instance out of `failed` (section 6).
		s.recordServerRunning(ctx, row, launched)
		return err
	}

	// Step 7: one transaction for both rows, so a database is never announced
	// by halves. A failure here leaves a healthy guest alone: the reconciler
	// finds a running domain whose engine answers and publishes it.
	endpoint := endpointName(row.Identifier, placement.vpcName)
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := s.instances.MarkManagedRunningInTx(ctx, tx, row.InstanceID, launched); err != nil {
			return err
		}
		if _, err := tx.MarkDBInstanceAvailable(ctx, sqlc.MarkDBInstanceAvailableParams{
			ID:        row.ID,
			Endpoint:  sql.NullString{String: endpoint, Valid: true},
			UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark DB instance %q available: %w", row.ID, err)
		}
		return nil
	}); err != nil {
		s.logger.ErrorContext(ctx, "dbinstance: the engine answers but the rows were not updated",
			"db_instance_id", row.ID, "task_id", t.ID, "error", err)
		return err
	}

	s.logger.InfoContext(ctx, "dbinstance: create finished",
		"db_instance_id", row.ID, "identifier", row.Identifier,
		"instance_id", row.InstanceID, "endpoint", endpoint, "task_id", t.ID)
	return nil
}

// recordServerRunning writes the VM's running state on the path where the
// create failed anyway. A failure to write is logged rather than returned: the
// caller is already reporting the reason the create gave up, and the instance
// pass reaches the same conclusion on its own next tick.
func (s *Service) recordServerRunning(
	ctx context.Context, row sqlc.DbInstance, launched instance.ManagedLaunched,
) {
	ctx = context.WithoutCancel(ctx)
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		return s.instances.MarkManagedRunningInTx(ctx, tx, row.InstanceID, launched)
	}); err != nil {
		s.logger.ErrorContext(ctx, "dbinstance: could not record the server as running",
			"db_instance_id", row.ID, "instance_id", row.InstanceID, "error", err)
	}
}

// placement is what the seed and the endpoint need from the network the guest
// sits on: the range pg_hba.conf admits, and the DNS domain the endpoint
// resolves under.
type placement struct {
	privateIP string
	vpcCIDR   string
	vpcName   string
	memoryMiB int
}

// placement reads the backing VM and the network behind it.
func (s *Service) placement(ctx context.Context, row sqlc.DbInstance) (placement, error) {
	server, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: row.InstanceID})
	if err != nil {
		return placement{}, fmt.Errorf("read the server of DB instance %q: %w", row.ID, err)
	}

	interfaces, err := s.store.ListInterfacesByInstances(ctx,
		sqlc.ListInterfacesByInstancesParams{InstanceIds: []string{row.InstanceID}})
	if err != nil {
		return placement{}, fmt.Errorf("read the network of DB instance %q: %w", row.ID, err)
	}
	if len(interfaces) == 0 {
		return placement{}, fmt.Errorf(
			"%w: the server of DB instance %q has no network interface", ErrBadTaskPayload, row.ID)
	}

	return placement{
		privateIP: interfaces[0].PrivateIp,
		vpcCIDR:   interfaces[0].VpcCidr,
		vpcName:   interfaces[0].VpcName,
		memoryMiB: int(server.MemoryMib),
	}, nil
}

// seed renders the cloud-init documents of the guest.
//
// The verifier comes from memory, never from a row: it is a password hash, and
// nothing in Kumo persists one (section 5). A create whose process restarted
// before this point cannot be finished, and says so rather than booting a
// database nobody can log in to.
func (s *Service) seed(
	ctx context.Context, row sqlc.DbInstance, place placement,
) (hypervisor.CloudInit, error) {
	verifier, ok := s.credentials.get(row.ID)
	if !ok {
		return hypervisor.CloudInit{}, fmt.Errorf("%w (DB instance %s)", ErrCredentialGone, row.ID)
	}

	request := dbcloudinit.Request{
		Identifier:    row.Identifier,
		AdminUsername: row.AdminUsername,
		DatabaseName:  row.DatabaseName,
		Verifier:      verifier,
		Port:          int(row.Port),
		VPCCIDR:       place.vpcCIDR,
		MemoryMiB:     place.memoryMiB,
		EngineVersion: row.EngineVersion,
	}
	// The create request validated the same fields, so a refusal here is a
	// disagreement between two layers rather than a bad request. It must still
	// stop the launch: the alternative is a guest that renders something the
	// shell reads differently than intended.
	if err := dbcloudinit.Validate(request); err != nil {
		return hypervisor.CloudInit{}, fmt.Errorf("the seed of DB instance %q: %w", row.ID, err)
	}

	userData, err := dbcloudinit.UserData(request)
	if err != nil {
		return hypervisor.CloudInit{}, fmt.Errorf("render the seed of DB instance %q: %w", row.ID, err)
	}

	s.logger.InfoContext(ctx, "dbinstance: seed rendered",
		"db_instance_id", row.ID, "instance_id", row.InstanceID)

	return hypervisor.CloudInit{
		MetaData:      cloudinit.MetaData(row.InstanceID, row.Identifier),
		UserData:      userData,
		NetworkConfig: cloudinit.NetworkConfig(),
	}, nil
}

// createDataDisk makes the blank disk the data directory lives on, and reports
// whether this run is the one that created it.
func (s *Service) createDataDisk(
	ctx context.Context, row sqlc.DbInstance, name string,
) (bool, error) {
	err := s.hv.CreateDataDisk(ctx, hypervisor.DataDiskSpec{
		DBInstanceID: row.ID,
		Pool:         s.instancesPool,
		SizeGiB:      row.StorageGb,
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, hypervisor.ErrAlreadyExists):
		// A requeued create finds the disk an interrupted run made, and it is
		// the normal case for one: the first-boot script leaves an initialized
		// disk alone, so this run simply finds the engine already up.
		s.logger.InfoContext(ctx, "dbinstance: adopting the data disk of an earlier attempt",
			"db_instance_id", row.ID, "volume", name)
		return false, nil
	default:
		return false, fmt.Errorf("create the data disk of DB instance %q: %w", row.ID, err)
	}
}

// deleteDataDisk removes the data disk on the rollback path. It is logged
// rather than returned: the caller is already reporting the failure that
// started the rollback.
func (s *Service) deleteDataDisk(ctx context.Context, dbInstanceID, name string) {
	if err := s.hv.DeleteVolume(ctx, s.instancesPool, name); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) {
		s.logger.ErrorContext(ctx, "dbinstance: rollback could not delete the data disk",
			"db_instance_id", dbInstanceID, "volume", name, "error", err)
	}
}

// awaitEngine polls the readiness probe until it answers, the DB instance is
// being deleted, or the timeout runs out (section 4 step 6).
func (s *Service) awaitEngine(ctx context.Context, dbInstanceID, address string) error {
	// The wall clock, not the service's: this is a wait on a guest rather than
	// arithmetic on a stored timestamp, the same way the instance service
	// waits for an address. A test that injects a clock is moving the
	// timestamps it asserts on, not asking a real wait to end sooner.
	deadline := time.Now().Add(s.readyTimeout)

	for {
		if err := s.probe(ctx, address); err == nil {
			s.markProbed(dbInstanceID)
			return nil
		}

		// A delete may arrive while this waits -- it is the way out of a
		// create that is stuck, so it does not take the lock this task holds.
		// Noticing it here is what keeps the delete from waiting out the whole
		// readiness timeout.
		if deleting, err := s.deleting(ctx, dbInstanceID); err == nil && deleting {
			return fmt.Errorf("%w (DB instance %s)", ErrCreateCancelled, dbInstanceID)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s did not answer on %s within %s",
				ErrEngineNotReady, dbInstanceID, address, s.readyTimeout)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the engine of DB instance %q: %w", dbInstanceID, ctx.Err())
		case <-time.After(s.probeInterval):
		}
	}
}

// deleting reports whether a DB instance has been asked to go away.
func (s *Service) deleting(ctx context.Context, dbInstanceID string) (bool, error) {
	row, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	return State(row.State) == StateDeleting, nil
}

// failCreate records why a create gave up.
//
// The VM is left exactly as it is. A failed create is not a reason to destroy a
// guest an operator may need to look at, and delete is the way out either way
// (section 4).
func (s *Service) failCreate(ctx context.Context, row sqlc.DbInstance, taskID string, cause error) {
	// Detached from the context: a task interrupted by a shutdown still has to
	// say why it failed.
	ctx = context.WithoutCancel(ctx)

	if _, err := s.store.SetDBInstanceState(ctx, sqlc.SetDBInstanceStateParams{
		ID:          row.ID,
		State:       string(StateFailed),
		StateReason: sql.NullString{String: cause.Error(), Valid: true},
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "dbinstance: could not record the create failure",
			"db_instance_id", row.ID, "error", err)
	}

	// The credential is dropped with the failure. A create that failed cannot
	// be resumed -- delete is the only way out -- so keeping a password hash in
	// memory for it would be holding a secret for nothing.
	s.credentials.forget(row.ID)

	s.logger.WarnContext(ctx, "dbinstance: create failed",
		"db_instance_id", row.ID, "identifier", row.Identifier,
		"instance_id", row.InstanceID, "task_id", taskID, "reason", cause.Error())
}

// DeleteHandler runs the db.delete task: the guest, then the data disk, then
// both rows, then the console log (docs/design/07-databases.md section 4).
//
// Every step tolerates a resource that is already gone, which is what makes the
// workflow safe to run twice -- and a task interrupted mid-flight is requeued
// on the next start.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
	dbInstanceID, err := decodeDelete(t)
	if err != nil {
		return err
	}

	// A blocking acquire rather than a try: a create may still be standing
	// down, and this task waits for it instead of failing.
	release, err := s.locks.Acquire(ctx, LockKey(dbInstanceID), t.Type)
	if err != nil {
		return fmt.Errorf("take the lock on DB instance %q: %w", dbInstanceID, err)
	}
	defer release()

	row, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.logger.InfoContext(ctx, "dbinstance: delete skipped, the DB instance is gone",
				"db_instance_id", dbInstanceID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get DB instance %q: %w", dbInstanceID, err)
	}

	// Undefine removes every volume the domain declares, which now includes the
	// data disk. The explicit delete below is the pass that still cleans up
	// when there was no domain left to read the disks from, exactly as the root
	// overlay's is.
	if err := s.instances.TeardownManaged(ctx, row.InstanceID, instance.ManagedByDatabases); err != nil {
		return err
	}
	if err := s.hv.DeleteVolume(ctx, s.instancesPool, hypervisor.DataDiskName(row.ID)); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("delete the data disk of DB instance %q: %w", row.ID, err)
	}

	// The db_instances row goes first: it references the instances row through
	// a foreign key, and the interfaces follow the instance through the
	// cascade.
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if _, err := tx.DeleteDBInstance(ctx, sqlc.DeleteDBInstanceParams{ID: row.ID}); err != nil {
			return fmt.Errorf("delete the row of DB instance %q: %w", row.ID, err)
		}
		return s.instances.DeleteManagedInTx(ctx, tx, row.InstanceID)
	}); err != nil {
		return err
	}

	s.instances.RemoveConsoleLog(ctx, row.InstanceID)
	s.credentials.forget(row.ID)
	s.probedAt.Delete(row.ID)

	s.logger.InfoContext(ctx, "dbinstance: deleted",
		"db_instance_id", row.ID, "identifier", row.Identifier,
		"instance_id", row.InstanceID, "task_id", t.ID)
	return nil
}

// decodeCreate reads the payload of a create task.
func decodeCreate(t worker.Task) (string, error) {
	var payload createPayload
	if err := t.Decode(&payload); err != nil {
		return "", err
	}
	if payload.DBInstanceID == "" {
		return "", fmt.Errorf("%w: create task %s names no DB instance", ErrBadTaskPayload, t.ID)
	}
	return payload.DBInstanceID, nil
}

// decodeDelete reads the payload of a delete task.
func decodeDelete(t worker.Task) (string, error) {
	var payload deletePayload
	if err := t.Decode(&payload); err != nil {
		return "", err
	}
	if payload.DBInstanceID == "" {
		return "", fmt.Errorf("%w: delete task %s names no DB instance", ErrBadTaskPayload, t.ID)
	}
	return payload.DBInstanceID, nil
}
