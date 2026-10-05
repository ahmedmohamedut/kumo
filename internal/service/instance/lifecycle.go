package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// Sentinel errors of the lifecycle workflows.
var (
	// ErrNoStart means a domain that was asked to boot never reported running.
	// The task fails on it and the row keeps the state it had, so the operator
	// can read the reason next to the instance instead of guessing.
	ErrNoStart = errors.New("instance: the domain did not start")
	// ErrNoShutdown means a graceful stop was still waiting when the watchdog
	// ran out. The guest is still up; only the operator stopping it is missing.
	ErrNoShutdown = errors.New("instance: the guest did not power off")
)

// Bounds of the console-output endpoint, from the tail_kib parameter of
// api/openapi.yaml.
const (
	// MinConsoleTailKiB is the smallest tail a client may ask for.
	MinConsoleTailKiB = 1
	// DefaultConsoleTailKiB is what a client that does not ask gets.
	DefaultConsoleTailKiB = 64
	// MaxConsoleTailKiB is the largest tail: a MiB of serial console is more than
	// a terminal wants, and the whole log stays on the host anyway.
	MaxConsoleTailKiB = 1024
)

// ConsoleOutput is the tail of an instance's serial console, the shape
// api/openapi.yaml promises for GET /instances/{id}/console-output.
type ConsoleOutput struct {
	InstanceID string `json:"instance_id"`
	Output     string `json:"output"`
}

// actionPayload is the task input of every lifecycle action.
//
// The instance id is on the task row as well; it is repeated here so a workflow
// can be handed a task without a database read just to learn what it works on.
// Force is here rather than guessed because it decides whether the action asks
// the guest or overrides it, and a retry has to make the same choice.
type actionPayload struct {
	InstanceID string `json:"instance_id"`
	Force      bool   `json:"force,omitempty"`
}

// action is one lifecycle request: the states it may be queued from, the state it
// writes when it is accepted, and the task the worker runs.
type action struct {
	// instanceID is the resource every field below is about.
	instanceID string
	// taskType is the task the worker runs.
	taskType string
	// from are the states the action may be queued from. It is the state machine
	// of docs/design/00-architecture.md §4, not a guess: stopping a stopped
	// instance is a request for something that is already true.
	from []State
	// next is the state written in the same transaction that creates the task. It
	// is empty for an action with no transitional state in that machine: a start
	// leaves the row saying stopped until the guest is up, because there pending
	// means a launch.
	next State
	// settled are the states that already mean the action is done. A terminate
	// answers for them without queueing a second task, which is what makes
	// repeating the call safe.
	settled []State
	// force records ?force=true in the task payload.
	force bool
}

// allows reports whether the action may be queued from state.
func (a action) allows(state State) bool {
	for _, candidate := range a.from {
		if candidate == state {
			return true
		}
	}
	return false
}

// isSettled reports whether state already means the action is done.
func (a action) isSettled(state State) bool {
	for _, candidate := range a.settled {
		if candidate == state {
			return true
		}
	}
	return false
}

// expects names the states the action can start from, for the conflict message.
func (a action) expects() string {
	names := make([]string, 0, len(a.from))
	for _, state := range a.from {
		names = append(names, string(state))
	}
	return strings.Join(names, " or ")
}

// Start queues the boot of a stopped instance.
//
// It answers 202 with the instance and a task: powering a domain up takes as long
// as the guest takes to boot, which is not an HTTP request.
func (s *Service) Start(ctx context.Context, instanceID string) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceStart,
		from:       []State{StateStopped},
	})
}

// Stop queues a shutdown. force stops the domain without asking the guest; a
// graceful stop waits for the guest to power off and leaves the row in stopping
// until it does (docs/design/04-libvirt-integration.md §3).
func (s *Service) Stop(ctx context.Context, instanceID string, force bool) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceStop,
		// stopping is a source as well as a destination: a graceful stop whose
		// guest ignored the request leaves the row there, and the way out of it is
		// another stop, forced or not.
		from:  []State{StateRunning, StateStopping},
		next:  StateStopping,
		force: force,
	})
}

// Reboot queues a restart. force resets the domain instead of asking the guest.
func (s *Service) Reboot(ctx context.Context, instanceID string, force bool) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceReboot,
		from:       []State{StateRunning},
		force:      force,
	})
}

// Terminate queues the removal of an instance: the domain goes, both volumes go
// and the row stays as history (docs/design/04-libvirt-integration.md §3).
//
// It is allowed from every state a live instance can be in, because it is also
// the way out of one that is stuck; a repeated call for an instance that is already
// terminated answers without queueing anything.
func (s *Service) Terminate(ctx context.Context, instanceID string) (Instance, error) {
	return s.queue(ctx, action{
		instanceID: instanceID,
		taskType:   task.TypeInstanceTerminate,
		from: []State{
			StatePending, StateRunning, StateStopping, StateStopped, StateTerminating, StateError,
		},
		next:    StateTerminating,
		settled: []State{StateTerminated},
	})
}

// queue is the half of every lifecycle endpoint that touches the database: it
// validates the request, writes the state the action implies and creates its task
// in one transaction.
//
// Three guards stand between a request and a task, and each of them is a conflict
// rather than an error:
//
//   - the per-instance lock, so a request that arrives while a workflow is running
//     on the instance does not queue a second one to run behind it;
//   - the states the action may start from, which is the state machine rather than
//     a guess about what an operator probably meant;
//   - an unfinished task, which closes the window between accepting an action and
//     the worker picking it up. The row has not moved yet, so without this a
//     second stop would pass the state check while the first one is still queued
//     and the instance would be stopped twice.
func (s *Service) queue(ctx context.Context, act action) (Instance, error) {
	// Read before locking, so an unknown id is a 404 rather than a lock on a key
	// nothing owns. The state is re-read inside the transaction under the lock.
	if _, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: act.instanceID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Instance{}, fmt.Errorf("%w: no instance %q", service.ErrNotFound, act.instanceID)
		}
		return Instance{}, fmt.Errorf("get instance: %w", err)
	}

	release, err := s.locks.TryAcquire(LockKey(act.instanceID), act.taskType)
	if err != nil {
		holder, held := s.locks.Holder(LockKey(act.instanceID))
		if !held {
			// Unreachable in practice: a held lock is what made TryAcquire fail.
			// The message still has to read well if it ever happens.
			holder = "another operation"
		}
		return Instance{}, fmt.Errorf("%w: instance %q is busy with %s",
			service.ErrConflict, act.instanceID, holder)
	}
	defer release()

	var taskID *string
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetInstance(ctx, sqlc.GetInstanceParams{ID: act.instanceID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no instance %q", service.ErrNotFound, act.instanceID)
			}
			return fmt.Errorf("get instance: %w", err)
		}

		switch state := State(row.State); {
		case act.isSettled(state):
			// Already done: answer with the row and queue nothing.
			return nil
		case !act.allows(state):
			return fmt.Errorf("%w: instance %q is %s, and %s only applies to a %s instance",
				service.ErrConflict, row.ID, state, act.taskType, act.expects())
		}

		if err := s.assertNoTaskInFlight(ctx, tx, row.ID); err != nil {
			return err
		}

		if act.next != "" {
			row, err = tx.SetInstanceState(ctx, sqlc.SetInstanceStateParams{
				ID:          row.ID,
				State:       string(act.next),
				StateReason: sql.NullString{},
				UpdatedAt:   formatTime(s.now()),
			})
			if err != nil {
				return fmt.Errorf("mark instance %q %s: %w", row.ID, act.next, err)
			}
		}

		created, err := s.tasks.CreateInTx(ctx, tx, act.taskType, row.ID,
			actionPayload{InstanceID: row.ID, Force: act.force})
		if err != nil {
			return err
		}
		taskID = &created.ID
		return nil
	})
	if err != nil {
		return Instance{}, err
	}

	queued, err := s.Get(ctx, act.instanceID, false)
	if err != nil {
		return Instance{}, err
	}
	// The task just created is the one the console follows, and naming it here
	// rather than trusting "the most recent task" keeps the answer about the
	// request that was just accepted.
	queued.TaskID = taskID

	s.logger.InfoContext(ctx, "instance: lifecycle action queued",
		"instance_id", queued.ID, "action", act.taskType,
		"state", queued.State, "force", act.force, "task_id", deref(taskID))

	return queued, nil
}

// assertNoTaskInFlight refuses an action while an earlier one is unfinished.
//
// The latest task is enough to tell: an action is only queued when no unfinished
// task exists, so anything pending or running is the one a request is colliding
// with. The in-process lock cannot answer this on its own, because a task left
// running by a killed process is not held by any lock until the pool requeues it.
func (s *Service) assertNoTaskInFlight(ctx context.Context, tx *store.Tx, instanceID string) error {
	latest, err := tx.GetLatestInstanceTask(ctx, sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: instanceID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// An instance whose tasks were all deleted has nothing in flight.
			return nil
		}
		return fmt.Errorf("get the last task of instance %q: %w", instanceID, err)
	}

	switch task.State(latest.State) {
	case task.StatePending, task.StateRunning:
		return fmt.Errorf("%w: instance %q already has a %s task in progress",
			service.ErrConflict, instanceID, latest.Type)
	default:
		return nil
	}
}

// ConsoleOutput returns the tail of an instance's serial console log.
//
// The log is the file the domain writes through libvirt's <log> element
// (docs/design/04-libvirt-integration.md §5), so the bytes come from the backend
// rather than from a path this service reconstructs: an instance whose domain is
// gone has no console to show, and one whose domain was defined elsewhere still
// reports where its log is.
func (s *Service) ConsoleOutput(ctx context.Context, instanceID string, tailKiB int) (ConsoleOutput, error) {
	if tailKiB == 0 {
		tailKiB = DefaultConsoleTailKiB
	}
	if tailKiB < MinConsoleTailKiB || tailKiB > MaxConsoleTailKiB {
		return ConsoleOutput{}, &service.ValidationError{
			Message: "the console output request is not acceptable",
			Fields: []service.FieldError{{
				Field: "tail_kib",
				Reason: fmt.Sprintf("must be between %d and %d",
					MinConsoleTailKiB, MaxConsoleTailKiB),
			}},
		}
	}

	if _, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConsoleOutput{}, fmt.Errorf("%w: no instance %q", service.ErrNotFound, instanceID)
		}
		return ConsoleOutput{}, fmt.Errorf("get instance: %w", err)
	}

	out, err := s.hv.ConsoleOutput(ctx, instanceID, tailKiB*1024)
	if err != nil {
		if errors.Is(err, hypervisor.ErrNotFound) {
			return ConsoleOutput{}, fmt.Errorf("%w: instance %q has no console log",
				service.ErrNotFound, instanceID)
		}
		return ConsoleOutput{}, fmt.Errorf("read the console of instance %q: %w", instanceID, err)
	}

	return ConsoleOutput{InstanceID: instanceID, Output: string(out)}, nil
}

// StartHandler boots a domain that was shut off and records what came back.
//
// The row keeps saying stopped until the guest is up, so the state machine of
// docs/design/00-architecture.md §4 stays true: a start has no transitional state
// of its own, because pending there means a launch that has no domain yet.
func (s *Service) StartHandler(ctx context.Context, t worker.Task) error {
	payload, err := decodeAction(t)
	if err != nil {
		return err
	}

	row, release, skip, err := s.claimed(ctx, t, payload.InstanceID)
	defer release()
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if err := s.startInstance(ctx, row); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "instance: start finished",
		"instance_id", row.ID, "task_id", t.ID)
	return nil
}

// startInstance starts the domain, waits for it to be up and records the result.
func (s *Service) startInstance(ctx context.Context, row sqlc.Instance) error {
	err := s.hv.Start(ctx, row.ID)
	switch {
	case err == nil:
	case errors.Is(err, hypervisor.ErrNotFound):
		// The domain is gone, so this instance can never boot again. That is the
		// same conclusion the reconciler reaches when it finds a row without a
		// domain (docs/design/00-architecture.md §4), and it is recorded here
		// because this workflow is already holding the instance.
		s.recordFailure(ctx, row.ID, StateError,
			domainMissing+"the domain of instance "+row.ID+" is gone")
		return fmt.Errorf("start the domain of instance %q: %w", row.ID, err)
	case errors.Is(err, hypervisor.ErrInvalidState):
		// Somebody started the guest out of band. libvirt refuses to boot a
		// running domain, but the instance is in exactly the state the caller
		// wanted, so adopt it rather than report a conflict.
		info, getErr := s.hv.GetDomain(ctx, row.ID)
		if getErr == nil && info.State == hypervisor.DomainStateRunning {
			return s.markRunning(ctx, row.ID, info)
		}
		return fmt.Errorf("start the domain of instance %q: %w", row.ID, err)
	default:
		return fmt.Errorf("start the domain of instance %q: %w", row.ID, err)
	}

	if err := s.awaitRunning(ctx, row.ID); err != nil {
		return err
	}

	info, err := s.hv.GetDomain(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("read the domain of instance %q: %w", row.ID, err)
	}
	return s.markRunning(ctx, row.ID, info)
}

// StopHandler powers an instance down and records the state that came out of it.
//
// A graceful stop waits for the guest, because ACPI is a request and the guest
// decides when to act on it; the wait is bounded by the watchdog of
// docs/design/04-libvirt-integration.md §3. A forced stop does not wait at all:
// overriding a guest is the point of force.
func (s *Service) StopHandler(ctx context.Context, t worker.Task) error {
	payload, err := decodeAction(t)
	if err != nil {
		return err
	}

	row, release, skip, err := s.claimed(ctx, t, payload.InstanceID)
	defer release()
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if err := s.stopInstance(ctx, row, payload.Force); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "instance: stop finished",
		"instance_id", row.ID, "task_id", t.ID, "force", payload.Force)
	return nil
}

// stopInstance asks the guest to power off, or overrides it, and waits for the
// domain to be off before the row says stopped.
func (s *Service) stopInstance(ctx context.Context, row sqlc.Instance, force bool) error {
	if force {
		// A domain that is already off is the outcome this step wants, so a
		// refusal to destroy it is not a failure.
		if err := s.hv.Destroy(ctx, row.ID); err != nil &&
			!errors.Is(err, hypervisor.ErrNotFound) && !errors.Is(err, hypervisor.ErrInvalidState) {
			return fmt.Errorf("destroy the domain of instance %q: %w", row.ID, err)
		}
		return s.markStopped(ctx, row.ID)
	}

	if err := s.hv.Shutdown(ctx, row.ID); err != nil {
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			// Nothing is running, which is what was asked for.
			s.logger.WarnContext(ctx, "instance: the domain of a stopping instance is gone",
				"instance_id", row.ID)
		case !errors.Is(err, hypervisor.ErrInvalidState):
			return fmt.Errorf("shut the domain of instance %q down: %w", row.ID, err)
		}
	}

	// Wait either way: after a request the guest was not running, and after a
	// domain that is gone there is nothing left to wait for.
	if err := s.awaitStopped(ctx, row.ID, s.stopTimeout); err != nil {
		return err
	}
	return s.markStopped(ctx, row.ID)
}

// RebootHandler restarts a domain.
//
// Nothing is written when the reboot goes through: the instance was running and
// is running again, and a guest that does not come back is something the
// reconciler (M4.5) reports rather than something this task guesses at.
func (s *Service) RebootHandler(ctx context.Context, t worker.Task) error {
	payload, err := decodeAction(t)
	if err != nil {
		return err
	}

	row, release, skip, err := s.claimed(ctx, t, payload.InstanceID)
	defer release()
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if err := s.rebootInstance(ctx, row, payload.Force); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "instance: reboot finished",
		"instance_id", row.ID, "task_id", t.ID, "force", payload.Force)
	return nil
}

// rebootInstance reboots the domain, or resets it when the request was forced.
func (s *Service) rebootInstance(ctx context.Context, row sqlc.Instance, force bool) error {
	var err error
	if force {
		err = s.hv.Reset(ctx, row.ID)
	} else {
		err = s.hv.Reboot(ctx, row.ID)
	}

	switch {
	case err == nil:
		return nil
	case errors.Is(err, hypervisor.ErrNotFound):
		s.recordFailure(ctx, row.ID, StateError,
			domainMissing+"the domain of instance "+row.ID+" is gone")
		return fmt.Errorf("reboot the domain of instance %q: %w", row.ID, err)
	case errors.Is(err, hypervisor.ErrInvalidState):
		// The guest is not running, so the row must stop saying that it is. What
		// the state becomes is the hypervisor's answer to the next read.
		info, getErr := s.hv.GetDomain(ctx, row.ID)
		if getErr != nil {
			return fmt.Errorf("read the domain of instance %q: %w", row.ID, getErr)
		}
		if err := s.markStopped(ctx, row.ID); err != nil {
			return err
		}
		return fmt.Errorf("reboot the domain of instance %q, which is %s: %w",
			row.ID, info.State, err)
	default:
		return fmt.Errorf("reboot the domain of instance %q: %w", row.ID, err)
	}
}

// TerminateHandler destroys the domain and deletes the volumes of an instance,
// then records the termination (docs/design/04-libvirt-integration.md §3).
//
// Every step tolerates a resource that is already gone. That is what makes the
// workflow safe to run twice, which matters because a task interrupted
// mid-flight is requeued on the next start (worker.Pool.Recover): a terminate that
// got as far as deleting the seed ISO has to be able to finish.
func (s *Service) TerminateHandler(ctx context.Context, t worker.Task) error {
	payload, err := decodeAction(t)
	if err != nil {
		return err
	}

	row, release, skip, err := s.claimed(ctx, t, payload.InstanceID)
	defer release()
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	if err := s.terminateInstance(ctx, row); err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "instance: terminated",
		"instance_id", row.ID, "task_id", t.ID)
	return nil
}

// terminateInstance removes the domain and the two volumes of an instance.
func (s *Service) terminateInstance(ctx context.Context, row sqlc.Instance) error {
	if err := s.hv.Destroy(ctx, row.ID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("destroy the domain of instance %q: %w", row.ID, err)
	}
	if err := s.hv.Undefine(ctx, row.ID); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
		return fmt.Errorf("undefine the domain of instance %q: %w", row.ID, err)
	}

	for _, name := range []string{hypervisor.RootDiskName(row.ID), hypervisor.SeedISOName(row.ID)} {
		if err := s.hv.DeleteVolume(ctx, s.instancesPool, name); err != nil &&
			!errors.Is(err, hypervisor.ErrNotFound) {
			return fmt.Errorf("delete the volume %q of instance %q: %w", name, row.ID, err)
		}
	}

	return s.markTerminated(ctx, row.ID)
}

// markRunning records a domain that is up again.
//
// The address is written only when the guest has one. A guest that boots into a
// network which has not handed out a lease yet is still running, and blanking the
// address the row already had would throw away the one an operator is using; the
// reconciler refreshes it on its next pass either way.
func (s *Service) markRunning(ctx context.Context, instanceID string, info hypervisor.DomainInfo) error {
	return s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if _, err := tx.SetInstanceState(ctx, sqlc.SetInstanceStateParams{
			ID:          instanceID,
			State:       string(StateRunning),
			StateReason: sql.NullString{},
			UpdatedAt:   formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark instance %q running: %w", instanceID, err)
		}

		if info.IP == "" {
			return nil
		}
		if _, err := tx.SetInstanceAddress(ctx, sqlc.SetInstanceAddressParams{
			ID:        instanceID,
			PrivateIp: sql.NullString{String: info.IP, Valid: true},
			UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("record the address of instance %q: %w", instanceID, err)
		}
		return nil
	})
}

// markStopped records a domain that is off.
func (s *Service) markStopped(ctx context.Context, instanceID string) error {
	_, err := s.store.SetInstanceState(ctx, sqlc.SetInstanceStateParams{
		ID:          instanceID,
		State:       string(StateStopped),
		StateReason: sql.NullString{},
		UpdatedAt:   formatTime(s.now()),
	})
	if err != nil {
		return fmt.Errorf("mark instance %q stopped: %w", instanceID, err)
	}
	return nil
}

// markTerminated records an instance whose domain and volumes are gone.
//
// The row stays, on purpose: it keeps the launch inputs for history and, through
// the partial unique index on the name, releases the name for reuse
// (docs/design/02-data-model.md). Only terminated releases the allocation the
// capacity report sums.
func (s *Service) markTerminated(ctx context.Context, instanceID string) error {
	now := formatTime(s.now())
	_, err := s.store.MarkInstanceTerminated(ctx, sqlc.MarkInstanceTerminatedParams{
		ID:           instanceID,
		TerminatedAt: sql.NullString{String: now, Valid: true},
		UpdatedAt:    now,
	})
	if err != nil {
		return fmt.Errorf("mark instance %q terminated: %w", instanceID, err)
	}
	return nil
}

// recordFailure writes the state and the reason of a workflow that could not do
// its work.
//
// A failure to write is logged and not returned: the caller returns the original
// error, and losing the row update must not replace the cause the operator has to
// read. The write is detached from the context, because a task interrupted by a
// shutdown still has to say why it failed.
func (s *Service) recordFailure(ctx context.Context, instanceID string, state State, reason string) {
	ctx = context.WithoutCancel(ctx)
	if _, err := s.store.SetInstanceState(ctx, sqlc.SetInstanceStateParams{
		ID:          instanceID,
		State:       string(state),
		StateReason: sql.NullString{String: reason, Valid: true},
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "instance: could not record the failure",
			"instance_id", instanceID, "state", state, "reason", reason, "error", err)
	}
}

// domainMissing is the reason prefix of an instance whose domain libvirt does not
// know, the same token the reconciler of docs/design/00-architecture.md §4 writes.
const domainMissing = "domain_missing: "

// awaitRunning waits for a domain that was just started to report running.
//
// Unlike the wait for an address in the launch workflow, this one does not fail
// when the guest takes no address: an instance that is up and unreachable is a
// running instance, and the reconciler refreshes the address on its next pass.
func (s *Service) awaitRunning(ctx context.Context, instanceID string) error {
	deadline := time.Now().Add(s.pollTimeout)

	for {
		info, err := s.hv.GetDomain(ctx, instanceID)
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			return fmt.Errorf("%w: the domain of instance %q is gone", ErrNoStart, instanceID)
		case err != nil:
			s.logger.WarnContext(ctx, "instance: could not read the domain while starting",
				"instance_id", instanceID, "error", err)
		case info.State == hypervisor.DomainStateRunning:
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w: instance %q was still not running after %s",
				ErrNoStart, instanceID, s.pollTimeout)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the domain of instance %q to start: %w", instanceID, ctx.Err())
		case <-time.After(s.pollInterval):
		}
	}
}

// awaitStopped waits for an ACPI shutdown to reach the guest.
//
// A domain that is gone counts as stopped: nothing is running any more, which is
// what the row has to say. A guest that ignores the request is not a failure of
// the wait but of the guest, and the watchdog turns it into an error the operator
// can act on with force rather than an instance stuck in stopping forever.
func (s *Service) awaitStopped(ctx context.Context, instanceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		info, err := s.hv.GetDomain(ctx, instanceID)
		switch {
		case errors.Is(err, hypervisor.ErrNotFound):
			return nil
		case err != nil:
			s.logger.WarnContext(ctx, "instance: could not read the domain while stopping",
				"instance_id", instanceID, "error", err)
		case info.State != hypervisor.DomainStateRunning:
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%w: instance %q was still running after %s",
				ErrNoShutdown, instanceID, timeout)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the domain of instance %q to stop: %w", instanceID, ctx.Err())
		case <-time.After(s.pollInterval):
		}
	}
}

// decodeAction reads the payload of a lifecycle task.
func decodeAction(t worker.Task) (actionPayload, error) {
	var payload actionPayload
	if err := t.Decode(&payload); err != nil {
		return actionPayload{}, err
	}
	if payload.InstanceID == "" {
		return actionPayload{}, fmt.Errorf("%w: task %s names no instance", ErrBadTaskPayload, t.ID)
	}
	return payload, nil
}

// claimed is the preamble every lifecycle workflow shares: it takes the lock of the
// instance and reads its row.
//
// skip is true when the task has nothing left to do: the row is gone, or the
// instance is on its way out. A launch that was queued before a terminate has to
// see that, because the volumes it would boot from are being deleted.
//
// release is always safe to call, including when err is set.
func (s *Service) claimed(ctx context.Context, t worker.Task, instanceID string) (sqlc.Instance, func(), bool, error) {
	// A worker waits for the lock rather than giving up: two tasks for one
	// instance have to run in some order, and the queue will run them both.
	release, err := s.locks.Acquire(ctx, LockKey(instanceID), t.Type)
	if err != nil {
		return sqlc.Instance{}, func() {}, false,
			fmt.Errorf("take the lock on instance %q: %w", instanceID, err)
	}

	row, err := s.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		release()
		if errors.Is(err, sql.ErrNoRows) {
			s.logger.InfoContext(ctx, "instance: task skipped, the instance is gone",
				"instance_id", instanceID, "task_id", t.ID)
			return sqlc.Instance{}, func() {}, true, nil
		}
		return sqlc.Instance{}, func() {}, false, fmt.Errorf("get instance %q: %w", instanceID, err)
	}

	switch state := State(row.State); {
	case state == StateTerminated:
		// Everything is already gone; the task is a repeat of work that finished.
		s.logger.InfoContext(ctx, "instance: task skipped, the instance is terminated",
			"instance_id", instanceID, "task_id", t.ID)
		release()
		return sqlc.Instance{}, func() {}, true, nil
	case state == StateTerminating && t.Type != task.TypeInstanceTerminate:
		// A terminate is already removing this instance. A stop or a reboot of a
		// domain that is about to disappear is not something to do, and a launch
		// must not rebuild what the terminate is deleting.
		s.logger.InfoContext(ctx, "instance: task skipped, the instance is being terminated",
			"instance_id", instanceID, "task_id", t.ID, "task_type", t.Type)
		release()
		return sqlc.Instance{}, func() {}, true, nil
	}

	return row, release, false, nil
}
