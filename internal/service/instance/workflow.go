package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/cloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// ErrBadTaskPayload means a task did not name the instance it works on.
var ErrBadTaskPayload = errors.New("instance: the task payload is not acceptable")

// ErrNoAddress means the guest never took a DHCP lease inside the timeout. The
// launch fails rather than publishing an instance nobody can reach.
var ErrNoAddress = errors.New("instance: the guest took no address")

// domainArch is the only architecture the domain template of
// docs/design/04-libvirt-integration.md §5 writes.
const domainArch = "x86_64"

// LaunchHandler runs the instance.launch task: it makes the volumes, defines the
// domain, waits for the guest to have an address and records the result. It runs
// in the worker, never in an HTTP handler (AGENTS.md architecture rule 4).
//
// The steps and their failure behavior, from
// docs/design/04-libvirt-integration.md §3:
//
//  1. create the root disk, a copy-on-write overlay on the image;
//  2. create the seed ISO with the rendered cloud-init documents;
//  3. define and start the domain;
//  4. poll for a running domain with an address, then record it.
//
// A failure at any step removes everything the steps before it created: the
// domain, the seed ISO and the root disk. The row stays, in state error with the
// reason, because a launch somebody can read the reason of is worth more than a
// launch that left nothing behind.
//
// Every step tolerates its own resource already existing, because the worker
// requeues a task it did not finish (worker.Recover) and the second run has to
// adopt what the first one made rather than fail on it.
func (s *Service) LaunchHandler(ctx context.Context, t worker.Task) error {
	var payload launchPayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.InstanceID == "" {
		return fmt.Errorf("%w: launch task %s names no instance", ErrBadTaskPayload, t.ID)
	}

	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: payload.InstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The row is gone, so there is nothing to launch and nothing to clean
			// up: the instance was deleted while the task was queued.
			s.logger.InfoContext(ctx, "instance: launch skipped, the instance is gone",
				"instance_id", payload.InstanceID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get instance %q: %w", payload.InstanceID, err)
	}

	// A launch that already finished is a no-op. Without this a requeued task
	// would create a second domain for the same instance.
	if State(row.State) == StateRunning {
		s.logger.InfoContext(ctx, "instance: launch already running",
			"instance_id", row.ID, "task_id", t.ID)
		return nil
	}

	// Everything created from here on is recorded as it appears, so the rollback
	// removes exactly what exists rather than what the steps would have created.
	created := rollback{instanceID: row.ID}

	info, err := s.launch(ctx, row, &created)
	if err != nil {
		if ctx.Err() != nil {
			// The context is done, which on this path means the process is shutting
			// down rather than that the launch failed. The worker leaves such a
			// task 'running' and the next Start requeues it (worker.Pool.run), so
			// the right thing here is to leave the row pending and whatever was
			// created in place: the requeued task adopts it. Marking the row error
			// would report a shutdown as something the user has to act on, and
			// deleting the volumes would throw away work that is fine.
			s.logger.WarnContext(ctx, "instance: launch interrupted by shutdown",
				"instance_id", row.ID, "task_id", t.ID, "error", err)
			return err
		}

		s.failLaunch(ctx, row.ID, t.ID, err)
		s.undo(ctx, &created)
		return err
	}

	if _, err := s.store.MarkInstanceRunning(ctx, sqlc.MarkInstanceRunningParams{
		ID:          row.ID,
		LibvirtUuid: nullableString(info.UUID),
		MacAddress:  nullableString(info.MAC),
		PrivateIp:   nullableString(info.IP),
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		// The guest is up but the row does not say so. That is not a launch
		// failure worth tearing down: the domain is running and the reconciler
		// will bring the row in line on its next pass.
		s.logger.ErrorContext(ctx, "instance: the guest is running but the row was not updated",
			"instance_id", row.ID, "task_id", t.ID, "error", err)
		return fmt.Errorf("record instance %q as running: %w", row.ID, err)
	}

	s.logger.InfoContext(ctx, "instance: launch finished",
		"instance_id", row.ID, "task_id", t.ID, "ip", info.IP, "mac", info.MAC)

	return nil
}

// launch runs the four steps of the workflow and reports the domain it reached.
func (s *Service) launch(
	ctx context.Context, row sqlc.Instance, created *rollback,
) (hypervisor.DomainInfo, error) {
	source, err := s.images.LaunchSource(ctx, row.ImageID)
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("read the image %q: %w", row.ImageID, err)
	}

	if err := s.createRootDisk(ctx, row, source, created); err != nil {
		return hypervisor.DomainInfo{}, err
	}
	if err := s.createSeed(ctx, row, source, created); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	if _, err := s.startDomain(ctx, row, created); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	// Step 4: the address only exists once the guest has asked for one, so this
	// is a wait rather than a read. A guest that never takes one fails the
	// launch, because an instance without an address is one nobody can reach.
	if err := s.awaitAddress(ctx, row.ID); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	// Re-read after the wait: the address arrived in this call, not in the one
	// that started the domain.
	info, err := s.hv.GetDomain(ctx, row.ID)
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("read the domain of instance %q: %w", row.ID, err)
	}
	return info, nil
}

// createRootDisk makes the copy-on-write overlay the instance boots from.
func (s *Service) createRootDisk(
	ctx context.Context, row sqlc.Instance, source image.LaunchSource, created *rollback,
) error {
	name := hypervisor.RootDiskName(row.ID)

	err := s.hv.CreateRootDisk(ctx, hypervisor.RootDiskSpec{
		InstanceID:   row.ID,
		SourceVolume: source.VolumeName,
		Pool:         s.instancesPool,
		SizeGiB:      row.RootDiskGb,
	})
	switch {
	case err == nil:
		created.volumes = append(created.volumes, name)
		return nil
	case errors.Is(err, hypervisor.ErrAlreadyExists):
		// A requeued launch finds the overlay the interrupted run made. It has to
		// exist for the domain to start, so adopt it and let the rollback remove
		// it if this run fails.
		created.volumes = append(created.volumes, name)
		s.logger.InfoContext(ctx, "instance: adopting the root disk of an earlier attempt",
			"instance_id", row.ID, "volume", name)
		return nil
	default:
		return fmt.Errorf("create the root disk of instance %q: %w", row.ID, err)
	}
}

// createSeed builds the NoCloud seed and puts it in the instances pool.
func (s *Service) createSeed(
	ctx context.Context, row sqlc.Instance, source image.LaunchSource, created *rollback,
) error {
	documents, err := s.seedDocuments(ctx, row, source)
	if err != nil {
		return err
	}

	name := hypervisor.SeedISOName(row.ID)
	err = s.hv.CreateSeedISO(ctx, s.instancesPool, name, documents)
	switch {
	case err == nil:
		created.volumes = append(created.volumes, name)
		return nil
	case errors.Is(err, hypervisor.ErrAlreadyExists):
		created.volumes = append(created.volumes, name)
		s.logger.InfoContext(ctx, "instance: adopting the seed of an earlier attempt",
			"instance_id", row.ID, "volume", name)
		return nil
	default:
		return fmt.Errorf("create the seed of instance %q: %w", row.ID, err)
	}
}

// seedDocuments renders the three cloud-init documents the seed ISO carries.
//
// The rendering lives in internal/service/cloudinit, so the rules about which key
// goes into which user-data shape are decided once (docs/design/04-libvirt-integration.md
// §4) and a backend never has to know what a cloud-config is.
func (s *Service) seedDocuments(
	ctx context.Context, row sqlc.Instance, source image.LaunchSource,
) (hypervisor.CloudInit, error) {
	userData := ""
	if row.UserData.Valid {
		userData = row.UserData.String
	}

	request := cloudinit.Request{
		UserData:    userData,
		DefaultUser: source.DefaultUser,
	}

	// A key pair named on the row is the one whose public key the guest gets. It
	// is read here rather than kept on the row: a key pair can be deleted while a
	// launch is queued, and a launch that has not started yet should still
	// deliver the key it was promised.
	if row.KeyPairID.Valid && row.KeyPairID.String != "" {
		pair, err := s.keyPairs.Get(ctx, row.KeyPairID.String)
		if err != nil {
			if errors.Is(err, service.ErrNotFound) {
				return hypervisor.CloudInit{}, fmt.Errorf(
					"the key pair %q of instance %q is gone: %w", row.KeyPairID.String, row.ID,
					ErrBadTaskPayload)
			}
			return hypervisor.CloudInit{}, fmt.Errorf("read the key pair %q: %w", row.KeyPairID.String, err)
		}
		request.PublicKey = pair.PublicKey
	}

	userData, err := cloudinit.UserData(request)
	if err != nil {
		return hypervisor.CloudInit{}, fmt.Errorf("render the user data of instance %q: %w", row.ID, err)
	}

	return hypervisor.CloudInit{
		MetaData:      cloudinit.MetaData(row.ID, row.Name),
		UserData:      userData,
		NetworkConfig: cloudinit.NetworkConfig(),
	}, nil
}

// startDomain resolves the volume paths, writes the domain XML and boots the
// guest.
func (s *Service) startDomain(
	ctx context.Context, row sqlc.Instance, created *rollback,
) (hypervisor.DomainInfo, error) {
	rootDisk := hypervisor.RootDiskName(row.ID)
	seed := hypervisor.SeedISOName(row.ID)

	rootPath, err := s.volumes.VolumePath(ctx, s.instancesPool, rootDisk)
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("resolve the root disk of instance %q: %w", row.ID, err)
	}
	seedPath, err := s.volumes.VolumePath(ctx, s.instancesPool, seed)
	if err != nil {
		return hypervisor.DomainInfo{}, fmt.Errorf("resolve the seed of instance %q: %w", row.ID, err)
	}
	if err := s.ensureConsoleDir(); err != nil {
		return hypervisor.DomainInfo{}, err
	}

	spec := hypervisor.DomainSpec{
		ID:           row.ID,
		Name:         row.Name,
		VCPUs:        int(row.Vcpus),
		MemoryMiB:    int(row.MemoryMib),
		Arch:         domainArch,
		RootDiskPool: s.instancesPool,
		RootDiskPath: rootPath,
		SeedISOPath:  seedPath,
		// The log is the tail the console-output endpoint reads, so it is written
		// next to the data dir rather than into a pool: nothing deletes it with
		// the volumes, and the console of an instance that failed to launch is
		// worth reading afterwards.
		ConsoleLogPath: s.consoleLogPath(row.ID),
		Network:        s.network,
	}

	info, err := s.hv.DefineAndStart(ctx, spec)
	switch {
	case err == nil:
		created.domain = true
		return info, nil
	case errors.Is(err, hypervisor.ErrAlreadyExists):
		// An interrupted run defined the domain already. Adopting it is safe
		// because the domain name is the instance id, so nothing else can own
		// that name.
		created.domain = true
		s.logger.InfoContext(ctx, "instance: adopting the domain of an earlier attempt",
			"instance_id", row.ID)
		existing, getErr := s.hv.GetDomain(ctx, row.ID)
		if getErr != nil {
			return hypervisor.DomainInfo{}, fmt.Errorf("read the domain of instance %q: %w", row.ID, getErr)
		}
		return existing, nil
	default:
		return hypervisor.DomainInfo{}, fmt.Errorf("define the domain of instance %q: %w", row.ID, err)
	}
}

// awaitAddress waits for the guest to have an address, which is what
// docs/design/04-libvirt-integration.md §3 step 4 asks for: a running domain
// with a DHCP lease.
//
// A domain that is gone or no longer running while this waits is a failure, not
// something to keep waiting for: the guest is not coming back on its own.
func (s *Service) awaitAddress(ctx context.Context, instanceID string) error {
	deadline := time.Now().Add(s.pollTimeout)
	interval := s.pollInterval

	for {
		info, err := s.hv.GetDomain(ctx, instanceID)
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			return fmt.Errorf("%w: the domain of instance %q is gone", ErrNoAddress, instanceID)
		case err != nil:
			// A backend that cannot answer right now is worth another try; one
			// that cannot answer for the whole timeout is not.
			s.logger.WarnContext(ctx, "instance: could not read the domain while waiting for an address",
				"instance_id", instanceID, "error", err)
		case info.State != hypervisor.DomainStateRunning:
			return fmt.Errorf("%w: the domain of instance %q is %s",
				ErrNoAddress, instanceID, info.State)
		case info.IP != "":
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w: instance %q had none after %s",
				ErrNoAddress, instanceID, s.pollTimeout)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the address of instance %q: %w", instanceID, ctx.Err())
		case <-time.After(interval):
		}
	}
}

// failLaunch marks the row failed with the reason. A failure to write the reason
// is logged and not returned: the caller already returns the original error, and
// losing the message must not hide the cause.
func (s *Service) failLaunch(ctx context.Context, instanceID, taskID string, cause error) {
	reason := cause.Error()

	// The state write has to survive a shutdown, because the task that carries it
	// is going to be requeued and would otherwise launch the instance again.
	ctx = context.WithoutCancel(ctx)
	if _, err := s.store.SetInstanceState(ctx, sqlc.SetInstanceStateParams{
		ID:          instanceID,
		State:       string(StateError),
		StateReason: sql.NullString{String: reason, Valid: true},
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "instance: could not record the launch failure",
			"instance_id", instanceID, "reason", reason, "error", err)
		return
	}

	s.logger.WarnContext(ctx, "instance: launch failed",
		"instance_id", instanceID, "task_id", taskID, "reason", reason)
}

// rollback remembers what a launch has created, so a failure can be undone in the
// reverse of the order the steps made it: the domain first, because it has the
// volumes attached, and the volumes newest first.
type rollback struct {
	// instanceID is the domain name to undefine. It is recorded up front because a
	// rollback that does not know what to remove cannot clean up.
	instanceID string
	// domain is true once the domain is defined.
	domain bool
	// volumes are the volume names created so far, oldest first.
	volumes []string
}

// undo removes what a failed launch left behind.
//
// It is best effort and only logged: it runs on the error path where the original
// error is what the caller must see. Every step tolerates a resource that is
// already gone, because a step that half succeeded is the normal case here.
func (s *Service) undo(ctx context.Context, r *rollback) {
	if r.domain {
		if err := s.hv.Undefine(ctx, r.instanceID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
			s.logger.ErrorContext(ctx, "instance: rollback could not undefine the domain",
				"instance_id", r.instanceID, "error", err)
		}
	}

	for i := len(r.volumes) - 1; i >= 0; i-- {
		name := r.volumes[i]
		if err := s.hv.DeleteVolume(ctx, s.instancesPool, name); err != nil &&
			!errors.Is(err, hypervisor.ErrNotFound) {
			s.logger.ErrorContext(ctx, "instance: rollback could not delete a volume",
				"instance_id", r.instanceID, "volume", name, "error", err)
		}
	}
}

// ensureDir creates a directory if it is missing.
func ensureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("create the directory %s: %w", path, err)
	}
	return nil
}

// Handler returns the task types this workflow answers, for the worker registry.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeInstanceLaunch: worker.HandlerFunc(s.LaunchHandler),
	}
}
