package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// This file is the seam another Kumo module launches and runs a VM through.
//
// The databases module needs a guest, not an instance: the row it creates is
// hidden from every Instances page, and the resource a user acts on is the DB
// instance in front of it (docs/design/07-databases.md R7). What it must not
// have is a second copy of the launch: the address allocation, the rollback,
// the adoption of a requeued task's volumes and the capacity arithmetic are
// what make a launch safe, and two implementations of them would drift.
//
// So the owning module keeps the decisions that are its own -- what the seed
// says, how big the data disk is, when the service inside the guest is ready --
// and borrows the rest from here.

// ManagedReservation is what an owning module asks for when it reserves a VM.
//
// It names no address and no MAC: those are allocated here, out of the subnet,
// in the same transaction as the row that claims them.
type ManagedReservation struct {
	// Name is the guest's name, and so its DNS label. For a DB instance it is
	// the identifier, because the label is the endpoint (R9).
	Name string
	// Owner is the module the row belongs to. It must not be ManagedByNone: a
	// reservation with no owner would be a user's instance that no page shows.
	Owner ManagedBy
	// ImageID is the image the root disk overlays, already resolved and
	// available: this path does not second-guess the module's choice of image.
	ImageID string
	// InstanceType is the preset the guest's vCPUs and memory come from.
	InstanceType string
	// SubnetID attaches the guest to one subnet; empty means the default.
	SubnetID string
	// RootDiskGB is the overlay's size.
	RootDiskGB int
	// ExtraDiskGB is storage the module creates itself and the capacity check
	// must still account for -- a DB instance's data disk
	// (docs/design/07-databases.md section 4).
	ExtraDiskGB int
	// CreatedBy is the user the reservation is attributed to.
	CreatedBy string
}

// ManagedReserved is the VM a reservation produced, in the shape a module needs
// to render a seed and write its own row.
type ManagedReserved struct {
	InstanceID string
	Name       string
	VCPUs      int
	MemoryMiB  int
	RootDiskGB int
	PrivateIP  string
	MACAddress string
	SubnetID   string
	VPCID      string
}

// WithManagedReservation reserves a VM for an owning module and runs fn in the
// very same transaction.
//
// The whole point is that one transaction holds both halves: the instance row
// with its address and the module's own row with the task that will build the
// guest. A module that wrote its row afterwards could crash in between and
// leave a VM reserved that nothing owns -- invisible on the Instances pages by
// construction, and so invisible to everybody.
//
// The capacity check and the subnet lock are taken the way a launch takes them,
// before the transaction, because a request the node cannot hold should not
// hold a key another launch is waiting for.
func (s *Service) WithManagedReservation(
	ctx context.Context, in ManagedReservation,
	fn func(ctx context.Context, tx *store.Tx, reserved ManagedReserved) error,
) error {
	if in.Owner == ManagedByNone {
		return fmt.Errorf("%w: a managed reservation needs an owner", service.ErrInvalid)
	}
	if reason := validateName(in.Name); reason != "" {
		return &service.ValidationError{
			Message: "the reservation is not acceptable",
			Fields:  []service.FieldError{{Field: "name", Reason: reason}},
		}
	}

	preset, err := s.types.Get(ctx, in.InstanceType)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return &service.ValidationError{
				Message: "the reservation is not acceptable",
				Fields: []service.FieldError{{
					Field:  "instance_type",
					Reason: fmt.Sprintf("no instance type %q exists", in.InstanceType),
				}},
			}
		}
		return fmt.Errorf("read the instance type %q: %w", in.InstanceType, err)
	}

	// The data disk counts against the node as much as the root overlay does,
	// so the check is the sum rather than the launch's root disk alone.
	want := capacity.Build(preset.VCPUs, int64(preset.MemoryMiB),
		int64(in.RootDiskGB)+int64(in.ExtraDiskGB))
	if err := s.checkCapacity(ctx, want); err != nil {
		return err
	}

	allocSubnet, err := s.resolveLaunchSubnet(ctx, in.SubnetID)
	if err != nil {
		return err
	}

	release, err := s.subnetLocks.TryAcquire(subnet.LockKey(allocSubnet.ID), string(in.Owner))
	if err != nil {
		holder, held := s.subnetLocks.Holder(subnet.LockKey(allocSubnet.ID))
		if !held {
			holder = "another operation"
		}
		return fmt.Errorf("%w: the subnet %q is busy with %s",
			service.ErrConflict, allocSubnet.ID, holder)
	}
	defer release()

	// The same retry as a launch: the read of the allocated addresses and the
	// insert that claims one are two statements, so a collision only shows up
	// at write time and a fresh allocation is what resolves it.
	for attempt := 0; attempt < maxLaunchAllocationAttempts; attempt++ {
		err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
			reserved, err := s.reserveManaged(ctx, tx, in, preset.VCPUs, preset.MemoryMiB, allocSubnet)
			if err != nil {
				return err
			}
			return fn(ctx, tx, reserved)
		})
		if err == nil {
			return nil
		}
		if !errors.Is(err, errMACConflict) && !isDBLocked(err) {
			return err
		}
	}
	return err
}

// reserveManaged writes the instance row and its interface inside tx.
func (s *Service) reserveManaged(
	ctx context.Context, tx *store.Tx, in ManagedReservation,
	vcpus, memoryMiB int, allocSubnet sqlc.Subnet,
) (ManagedReserved, error) {
	now := formatTime(s.now())

	vpcRow, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: allocSubnet.VpcID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedReserved{}, fmt.Errorf("%w: the VPC %q of the subnet %q is gone",
				service.ErrConflict, allocSubnet.VpcID, allocSubnet.ID)
		}
		return ManagedReserved{}, fmt.Errorf("read the VPC %q: %w", allocSubnet.VpcID, err)
	}
	gateway, err := parseGateway(vpcRow)
	if err != nil {
		return ManagedReserved{}, err
	}

	allocated, err := tx.ListAllocatedIPsInSubnet(ctx,
		sqlc.ListAllocatedIPsInSubnetParams{SubnetID: allocSubnet.ID})
	if err != nil {
		return ManagedReserved{}, fmt.Errorf(
			"list the allocated addresses of subnet %q: %w", allocSubnet.ID, err)
	}
	ip, err := allocateIP(allocSubnet.Cidr, gateway, allocated)
	if err != nil {
		return ManagedReserved{}, err
	}
	mac := randomMAC()

	row, err := tx.CreateInstance(ctx, sqlc.CreateInstanceParams{
		ID:           newInstanceID(),
		Name:         in.Name,
		State:        string(StatePending),
		StateReason:  sql.NullString{},
		ImageID:      in.ImageID,
		InstanceType: in.InstanceType,
		Vcpus:        int64(vcpus),
		MemoryMib:    int64(memoryMiB),
		RootDiskGb:   int64(in.RootDiskGB),
		// No key pair and no user data: a managed guest is not a machine
		// anybody has a shell on, and its seed is rendered by the owning
		// module rather than stored (docs/design/07-databases.md section 5).
		KeyPairID:   sql.NullString{},
		KeyPairName: sql.NullString{},
		UserData:    sql.NullString{},
		MacAddress:  nullableString(mac),
		PrivateIp:   nullableString(ip.String()),
		CreatedBy:   nullableString(in.CreatedBy),
		LaunchedAt:  now,
		UpdatedAt:   now,
		ManagedBy:   string(in.Owner),
	})
	if err != nil {
		if isUniqueViolation(err) {
			// The backing row shares idx_instances_name_live with every user
			// instance, so an identifier that collides with an instance name
			// is refused here rather than producing two DNS labels
			// (docs/design/07-databases.md section 3).
			return ManagedReserved{}, fmt.Errorf("%w: an instance named %q already exists",
				service.ErrAlreadyExists, in.Name)
		}
		return ManagedReserved{}, fmt.Errorf("create the instance row: %w", err)
	}

	if _, err := tx.CreateNetworkInterface(ctx, sqlc.CreateNetworkInterfaceParams{
		ID:          id.NetworkInterface(),
		InstanceID:  row.ID,
		SubnetID:    allocSubnet.ID,
		DeviceIndex: 0,
		MacAddress:  mac,
		PrivateIp:   ip.String(),
		CreatedAt:   now,
	}); err != nil {
		if isUniqueViolation(err) {
			return ManagedReserved{}, fmt.Errorf("allocate the interface of %q: %w",
				row.ID, errMACConflict)
		}
		return ManagedReserved{}, fmt.Errorf("create the network interface: %w", err)
	}

	return ManagedReserved{
		InstanceID: row.ID,
		Name:       row.Name,
		VCPUs:      vcpus,
		MemoryMiB:  memoryMiB,
		RootDiskGB: in.RootDiskGB,
		PrivateIP:  ip.String(),
		MACAddress: mac,
		SubnetID:   allocSubnet.ID,
		VPCID:      vpcRow.ID,
	}, nil
}

// ManagedLaunch is what the owning module supplies for the build of the guest:
// the seed it decides the contents of, and a second disk it created itself.
type ManagedLaunch struct {
	// Seed renders the seed documents. It is a function rather than a value
	// because it is only called when the seed volume has to be created: a
	// requeued create must not have to render the seed again, and for a DB
	// instance it could not -- the credential it carries is gone once the
	// request that computed it has been answered
	// (docs/design/07-databases.md section 5).
	Seed func(ctx context.Context) (hypervisor.CloudInit, error)
	// DataDiskName is a volume in the instances pool to attach beside the root
	// disk, or empty for a guest with one disk. The module creates it; the
	// domain XML is where it has to be named.
	DataDiskName string
}

// ManagedLaunched is the guest a managed launch reached.
type ManagedLaunched struct {
	UUID       string
	MACAddress string
	PrivateIP  string
}

// LaunchManaged builds the guest of a reserved VM: the root overlay, the seed,
// the static DHCP host and the domain, then waits for an address.
//
// It is the launch workflow of docs/design/04-libvirt-integration.md section 3
// with two deltas -- the seed comes from the caller and a second disk may be
// attached -- and it keeps the two properties that matter: every step adopts
// what an earlier attempt left behind, and a failure removes what this run
// created. What it deliberately does not do is write the row's state: the
// owning module publishes its own resource and the VM in one transaction
// (MarkManagedRunningInTx), so a guest is never announced as ready by halves.
//
// The instance lock is held for the whole build, so the reconciler stays out of
// the way. The row is pending throughout, which a pass skips anyway.
func (s *Service) LaunchManaged(
	ctx context.Context, instanceID string, in ManagedLaunch,
) (ManagedLaunched, error) {
	release, err := s.locks.Acquire(ctx, LockKey(instanceID), "managed launch")
	if err != nil {
		return ManagedLaunched{}, fmt.Errorf("take the lock on instance %q: %w", instanceID, err)
	}
	defer release()

	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ManagedLaunched{}, fmt.Errorf("%w: no instance %q", service.ErrNotFound, instanceID)
		}
		return ManagedLaunched{}, fmt.Errorf("get instance %q: %w", instanceID, err)
	}
	if ManagedBy(row.ManagedBy) == ManagedByNone {
		return ManagedLaunched{}, fmt.Errorf(
			"%w: instance %q is not a managed VM", service.ErrConflict, instanceID)
	}

	created := rollback{instanceID: row.ID}
	launched, err := s.launchManaged(ctx, row, in, &created)
	if err != nil {
		if ctx.Err() != nil {
			// A shutdown, not a failure: the task is requeued and the next run
			// adopts everything this one made. Tearing it down would throw away
			// work that is fine (see LaunchHandler).
			s.logger.WarnContext(ctx, "instance: managed launch interrupted by shutdown",
				"instance_id", row.ID, "error", err)
			return ManagedLaunched{}, err
		}
		s.undo(ctx, &created)
		return ManagedLaunched{}, err
	}
	return launched, nil
}

// launchManaged runs the build steps and reports the domain it reached.
func (s *Service) launchManaged(
	ctx context.Context, row sqlc.Instance, in ManagedLaunch, created *rollback,
) (ManagedLaunched, error) {
	source, err := s.images.LaunchSource(ctx, row.ImageID)
	if err != nil {
		return ManagedLaunched{}, fmt.Errorf("read the image %q: %w", row.ImageID, err)
	}
	if err := s.createRootDisk(ctx, row, source, created); err != nil {
		return ManagedLaunched{}, err
	}
	if err := s.createManagedSeed(ctx, row, in, created); err != nil {
		return ManagedLaunched{}, err
	}

	network, err := s.interfaceNetwork(ctx, row.ID)
	if err != nil {
		return ManagedLaunched{}, err
	}
	if err := s.addDHCPHost(ctx, row, network, created); err != nil {
		return ManagedLaunched{}, err
	}

	if _, err := s.startManagedDomain(ctx, row, network, in, created); err != nil {
		return ManagedLaunched{}, err
	}
	if err := s.awaitAddress(ctx, row.ID); err != nil {
		return ManagedLaunched{}, err
	}

	info, err := s.hv.GetDomain(ctx, row.ID)
	if err != nil {
		return ManagedLaunched{}, fmt.Errorf("read the domain of instance %q: %w", row.ID, err)
	}
	return ManagedLaunched{UUID: info.UUID, MACAddress: info.MAC, PrivateIP: info.IP}, nil
}

// createManagedSeed writes the seed the module rendered, or adopts the one an
// earlier attempt wrote.
//
// The volume is looked up before anything is rendered, which is the opposite
// order from an ordinary launch. Rendering is the step that needs the
// credential, and a requeued create no longer has it: the seed that already
// exists is the one that carries it (docs/design/07-databases.md section 5).
func (s *Service) createManagedSeed(
	ctx context.Context, row sqlc.Instance, in ManagedLaunch, created *rollback,
) error {
	name := hypervisor.SeedISOName(row.ID)

	if _, err := s.volumes.VolumePath(ctx, s.instancesPool, name); err == nil {
		created.volumes = append(created.volumes, name)
		s.logger.InfoContext(ctx, "instance: adopting the seed of an earlier attempt",
			"instance_id", row.ID, "volume", name)
		return nil
	} else if !errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("resolve the seed of instance %q: %w", row.ID, err)
	}

	if in.Seed == nil {
		return fmt.Errorf("%w: the managed launch of %q supplied no seed",
			service.ErrInvalid, row.ID)
	}
	documents, err := in.Seed(ctx)
	if err != nil {
		return err
	}

	err = s.hv.CreateSeedISO(ctx, s.instancesPool, name, documents)
	switch {
	case err == nil, errors.Is(err, hypervisor.ErrAlreadyExists):
		created.volumes = append(created.volumes, name)
		return nil
	default:
		return fmt.Errorf("create the seed of instance %q: %w", row.ID, err)
	}
}

// startManagedDomain defines and starts the domain, with the module's second
// disk attached when there is one.
func (s *Service) startManagedDomain(
	ctx context.Context, row sqlc.Instance, network string, in ManagedLaunch, created *rollback,
) (hypervisor.DomainInfo, error) {
	rootPath, err := s.volumes.VolumePath(ctx, s.instancesPool, hypervisor.RootDiskName(row.ID))
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("resolve the root disk of instance %q: %w", row.ID, err)
	}
	seedPath, err := s.volumes.VolumePath(ctx, s.instancesPool, hypervisor.SeedISOName(row.ID))
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("resolve the seed of instance %q: %w", row.ID, err)
	}
	dataPath := ""
	if in.DataDiskName != "" {
		dataPath, err = s.volumes.VolumePath(ctx, s.instancesPool, in.DataDiskName)
		if err != nil {
			return hypervisor.DomainInfo{}, fmt.Errorf(
				"resolve the data disk of instance %q: %w", row.ID, err)
		}
	}
	if err := s.ensureConsoleDir(); err != nil {
		return hypervisor.DomainInfo{}, err
	}
	if err := s.ensureConsoleLog(row.ID); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	spec := hypervisor.DomainSpec{
		ID:             row.ID,
		Name:           row.Name,
		VCPUs:          int(row.Vcpus),
		MemoryMiB:      int(row.MemoryMib),
		Arch:           domainArch,
		RootDiskPool:   s.instancesPool,
		RootDiskPath:   rootPath,
		SeedISOPath:    seedPath,
		DataDiskPath:   dataPath,
		ConsoleLogPath: s.consoleLogPath(row.ID),
		Network:        network,
	}
	if row.MacAddress.Valid {
		spec.MACAddress = row.MacAddress.String
	}

	info, err := s.hv.DefineAndStart(ctx, spec)
	switch {
	case err == nil:
		created.domain = true
		return info, nil
	case errors.Is(err, hypervisor.ErrAlreadyExists):
		created.domain = true
		s.logger.InfoContext(ctx, "instance: adopting the domain of an earlier attempt",
			"instance_id", row.ID)
		existing, getErr := s.hv.GetDomain(ctx, row.ID)
		if getErr != nil {
			return hypervisor.DomainInfo{}, fmt.Errorf("read the domain of instance %q: %w", row.ID, getErr)
		}
		return existing, nil
	default:
		// DefineAndStart defines before it starts, so a failure in between
		// leaves a definition the rollback has to know about.
		if _, getErr := s.hv.GetDomain(ctx, row.ID); getErr == nil {
			created.domain = true
		}
		return hypervisor.DomainInfo{}, fmt.Errorf("define the domain of instance %q: %w", row.ID, err)
	}
}

// MarkManagedRunningInTx records the guest as running inside the owning
// module's transaction, so the VM and the resource in front of it become
// available together or not at all.
func (s *Service) MarkManagedRunningInTx(
	ctx context.Context, tx *store.Tx, instanceID string, launched ManagedLaunched,
) error {
	if _, err := tx.MarkInstanceRunning(ctx, sqlc.MarkInstanceRunningParams{
		ID:          instanceID,
		LibvirtUuid: nullableString(launched.UUID),
		MacAddress:  nullableString(launched.MACAddress),
		PrivateIp:   nullableString(launched.PrivateIP),
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		return fmt.Errorf("record instance %q as running: %w", instanceID, err)
	}
	return nil
}

// StartManaged, StopManaged and RebootManaged are the lifecycle actions the
// owning module takes on its own VM. They are the very same actions a user
// gets, through the very same guards -- the state machine, the per-instance
// lock and the unfinished-task check -- with the ownership test reversed: only
// a row this module owns may be acted on (docs/design/07-databases.md
// section 4).
func (s *Service) StartManaged(ctx context.Context, instanceID string, owner ManagedBy) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceStart,
		from:       []State{StateStopped},
		owner:      owner,
	})
}

// StopManaged asks the guest to power off. It is never forced: a database gets
// its shutdown checkpoint, which is the whole reason stop is an ACPI request
// here (docs/design/07-databases.md section 4).
func (s *Service) StopManaged(ctx context.Context, instanceID string, owner ManagedBy) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceStop,
		from:       []State{StateRunning, StateStopping},
		next:       StateStopping,
		owner:      owner,
	})
}

// RebootManaged restarts the guest, gracefully.
func (s *Service) RebootManaged(ctx context.Context, instanceID string, owner ManagedBy) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceReboot,
		from:       []State{StateRunning},
		owner:      owner,
	})
}

// TeardownManaged removes everything the hypervisor holds for a managed VM: the
// domain, its static DHCP host, its root overlay and its seed.
//
// The row stays. The owning module's row references it through a foreign key,
// so the two have to go in one transaction in that order, which is the module's
// to run (DeleteManagedInTx). Every step tolerates a resource that is already
// gone, because a requeued delete is the normal case.
func (s *Service) TeardownManaged(ctx context.Context, instanceID string, owner ManagedBy) error {
	release, err := s.locks.Acquire(ctx, LockKey(instanceID), "managed teardown")
	if err != nil {
		return fmt.Errorf("take the lock on instance %q: %w", instanceID, err)
	}
	defer release()

	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Already gone, which is where this is trying to end up.
			return nil
		}
		return fmt.Errorf("get instance %q: %w", instanceID, err)
	}
	if ManagedBy(row.ManagedBy) != owner {
		return s.errManaged(ctx, s.store, row, owner)
	}

	if err := s.hv.Destroy(ctx, row.ID); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) && !errors.Is(err, hypervisor.ErrInvalidState) {
		return fmt.Errorf("destroy the domain of instance %q: %w", row.ID, err)
	}
	if err := s.hv.Undefine(ctx, row.ID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("undefine the domain of instance %q: %w", row.ID, err)
	}
	if err := s.removeDHCPHost(ctx, row); err != nil {
		return err
	}
	for _, name := range []string{hypervisor.RootDiskName(row.ID), hypervisor.SeedISOName(row.ID)} {
		if err := s.hv.DeleteVolume(ctx, s.instancesPool, name); err != nil &&
			!errors.Is(err, hypervisor.ErrNotFound) {
			return fmt.Errorf("delete the volume %q of instance %q: %w", name, row.ID, err)
		}
	}
	return nil
}

// DeleteManagedInTx deletes the row of a managed VM and its tags inside the
// owning module's transaction.
func (s *Service) DeleteManagedInTx(ctx context.Context, tx *store.Tx, instanceID string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{
		ResourceID: instanceID,
	}); err != nil {
		return fmt.Errorf("delete the tags of instance %q: %w", instanceID, err)
	}
	if _, err := tx.DeleteInstance(ctx, sqlc.DeleteInstanceParams{ID: instanceID}); err != nil {
		return fmt.Errorf("delete the row of instance %q: %w", instanceID, err)
	}
	// Zero rows is not an error: a repeated delete must not report a failure it
	// has no way to undo.
	return nil
}

// RemoveConsoleLog deletes the serial log of a VM that is gone. It is best
// effort on purpose: the guest has already been removed by the time this runs,
// and failing the delete over a leftover file would report a deletion that
// happened as one that did not.
func (s *Service) RemoveConsoleLog(ctx context.Context, instanceID string) {
	if err := os.Remove(s.consoleLogPath(instanceID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.logger.WarnContext(ctx, "instance: could not remove the console log",
			"instance_id", instanceID, "error", err)
	}
}
