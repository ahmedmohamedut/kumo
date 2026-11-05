package dbinstance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Reconciliation keeps the DB instances in step with the VMs underneath them,
// the way the instance pass keeps those VMs in step with libvirt
// (docs/design/07-databases.md section 6).
//
// There is a third fact here that neither libvirt nor SQLite owns: whether the
// engine inside a running guest is accepting connections. A TCP connect is the
// whole of what Kumo can learn without an agent, and it is enough -- the
// first-boot script opens the VPC address only after the role and the database
// exist, so a port that answers means setup finished.
//
// A probe failure is never a reason to touch the guest. The pass reports; the
// operator or a reboot acts.

// probeFailedReason is the state_reason of a DB instance whose engine stopped
// answering. Like the instance pass's prefixes it is a token to grep for.
const probeFailedReason = "engine_not_responding: "

// serverFailedReason is the state_reason of a DB instance whose VM is in error:
// the domain is gone, or the guest crashed. The instance row carries the
// detail, and this one points at it.
const serverFailedReason = "server_failed: "

// Reconciliation counts what one pass saw and changed.
type Reconciliation struct {
	// Rows is how many DB instances the database held.
	Rows int
	// Probed is how many engines the pass dialed.
	Probed int
	// Updated is how many rows the pass wrote.
	Updated int
	// Skipped is how many rows the pass left to a task that owns them.
	Skipped int
}

// Reconcile runs one full pass over the DB instances.
//
// It is deliberately a second pass rather than a hook inside the instance one:
// the instance pass establishes what the guests are doing, and this one reads
// that result. Running after it in the same tick means a VM that an operator
// destroyed is already recorded as such by the time its database is judged.
func (s *Service) Reconcile(ctx context.Context) (Reconciliation, error) {
	rows, err := s.store.ListDBInstancesForReconcile(ctx)
	if err != nil {
		return Reconciliation{}, fmt.Errorf("reconcile: list the DB instances: %w", err)
	}

	report := Reconciliation{Rows: len(rows)}
	for _, row := range rows {
		changed, probed, skipped := s.reconcileRow(ctx, row)
		switch {
		case skipped:
			report.Skipped++
		case changed:
			report.Updated++
		}
		if probed {
			report.Probed++
		}
	}
	return report, nil
}

// reconcileRow brings one DB instance in line with its VM.
func (s *Service) reconcileRow(
	ctx context.Context, row sqlc.ListDBInstancesForReconcileRow,
) (changed, probed, skipped bool) {
	switch State(row.State) {
	case StateCreating, StateDeleting:
		// Owned by the task that wrote the state: a create has not opened the
		// engine's port yet, and a delete is removing it.
		return false, false, true
	}

	// A task that is working holds the lock for as long as it takes, so
	// failing to take it is the signal that somebody with more context owns
	// the row. The pass never waits: the next one is one interval away.
	release, err := s.locks.TryAcquire(LockKey(row.ID), "reconciler")
	if err != nil {
		return false, false, true
	}
	defer release()

	// A lifecycle action runs as a task on the VM rather than on the DB
	// instance, so the lock above says nothing about it. While one is in
	// flight the transitional state is the truth and the guest is mid-change:
	// judging it now would publish a database that is about to go down, or
	// stop one that is coming up.
	busy, err := s.serverBusy(ctx, row.InstanceID)
	if err != nil {
		s.logger.DebugContext(ctx, "reconcile: could not read the tasks of a DB instance's server",
			"db_instance_id", row.ID, "error", err)
		return false, false, true
	}
	if busy {
		return false, false, true
	}

	// The row may have been written between the list and the lock, so it is
	// read again before anything is decided from it.
	fresh, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: row.ID})
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.logger.ErrorContext(ctx, "reconcile: could not re-read the DB instance",
				"db_instance_id", row.ID, "error", err)
		}
		return false, false, true
	}
	switch State(fresh.State) {
	case StateCreating, StateDeleting:
		return false, false, true
	}
	state := State(fresh.State)

	switch instance.State(row.InstanceState) {
	case instance.StateRunning:
		return s.reconcileRunning(ctx, fresh, state,
			row.PrivateIp.String, row.VpcName.String)
	case instance.StateStopped:
		// The VM is off, whoever turned it off. A stop that was asked for ends
		// here, and so does one an operator did out of band.
		if state == StateStopped {
			return false, false, false
		}
		return s.setState(ctx, fresh.ID, StateStopped, "", state), false, false
	case instance.StateError:
		// The domain is gone or the guest crashed. The instance pass has
		// already recorded why on its own row.
		reason := serverFailedReason + "the server " + row.InstanceID + " is in error"
		if state == StateFailed && fresh.StateReason.String == reason {
			return false, false, false
		}
		return s.setState(ctx, fresh.ID, StateFailed, reason, state), false, false
	default:
		// pending, stopping, terminating: the VM is mid-change and its own
		// pass owns it.
		return false, false, true
	}
}

// reconcileRunning judges a DB instance whose VM is up, which is the only case
// the probe applies to.
func (s *Service) reconcileRunning(
	ctx context.Context, row sqlc.DbInstance, state State, privateIP, vpcName string,
) (changed, probed, skipped bool) {
	if privateIP == "" {
		// A guest that is running without a lease has nothing to dial yet.
		return false, false, true
	}
	if !s.probeDue(row.ID) {
		// One probe per DB instance per interval, however often the pass runs
		// (section 6, "Rate").
		return false, false, true
	}

	address := net.JoinHostPort(privateIP, strconv.Itoa(int(row.Port)))
	err := s.probe(ctx, address)
	s.markProbed(row.ID)

	if err == nil {
		if state == StateAvailable {
			return false, true, false
		}
		// A database that answers is available, whether it is coming back from
		// a start, a reboot, a stop somebody undid, or a probe that failed
		// once.
		s.logger.InfoContext(ctx, "reconcile: a DB instance is answering again",
			"db_instance_id", row.ID, "was_state", state)
		return s.publish(ctx, row, state, vpcName), true, false
	}

	reason := probeFailedReason + "nothing answers on " + address
	switch state {
	case StateAvailable:
		// It was available and is not: that is the one failure this pass
		// reports. The guest is left alone.
		s.logger.WarnContext(ctx, "reconcile: the engine of a DB instance stopped answering",
			"db_instance_id", row.ID, "address", address, "error", err)
		return s.setState(ctx, row.ID, StateFailed, reason, state), true, false
	case StateStarting, StateRebooting:
		// Still coming up. An engine is slower than a domain, so this is only
		// a failure once it has had the whole readiness timeout -- the same
		// bound a create waits under.
		if s.now().Sub(parseTime(row.UpdatedAt)) < s.readyTimeout {
			return false, true, true
		}
		s.logger.WarnContext(ctx, "reconcile: a DB instance never finished coming up",
			"db_instance_id", row.ID, "address", address, "was_state", state)
		return s.setState(ctx, row.ID, StateFailed,
			probeFailedReason+"nothing answered on "+address+" within "+
				s.readyTimeout.String(), state), true, false
	default:
		// failed, and still failed; or stopped with a guest that is up but
		// silent, which the state already describes well enough.
		return false, true, false
	}
}

// publish records a DB instance as available, writing the endpoint as well when
// it has none.
//
// A create that failed at the probe leaves the row with no endpoint, and the
// pass is then the only thing that will ever publish one. The name resolves
// already -- the static DHCP host was written during the build -- so a database
// the pass brings back without its endpoint would be one the console cannot
// offer a connection string for while it works perfectly.
func (s *Service) publish(
	ctx context.Context, row sqlc.DbInstance, was State, vpcName string,
) bool {
	if row.Endpoint.Valid && row.Endpoint.String != "" {
		return s.setState(ctx, row.ID, StateAvailable, "", was)
	}

	if _, err := s.store.MarkDBInstanceAvailable(ctx, sqlc.MarkDBInstanceAvailableParams{
		ID:        row.ID,
		Endpoint:  sql.NullString{String: endpointName(row.Identifier, vpcName), Valid: true},
		UpdatedAt: formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "reconcile: could not publish a DB instance",
			"db_instance_id", row.ID, "error", err)
		return false
	}

	s.logger.InfoContext(ctx, "reconcile: a DB instance changed state",
		"db_instance_id", row.ID, "was_state", was, "state", StateAvailable)
	return true
}

// setState writes a state and its reason, and reports whether it changed
// anything.
func (s *Service) setState(
	ctx context.Context, dbInstanceID string, next State, reason string, was State,
) bool {
	stateReason := sql.NullString{}
	if reason != "" {
		stateReason = sql.NullString{String: reason, Valid: true}
	}

	if _, err := s.store.SetDBInstanceState(ctx, sqlc.SetDBInstanceStateParams{
		ID:          dbInstanceID,
		State:       string(next),
		StateReason: stateReason,
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "reconcile: could not record the state of a DB instance",
			"db_instance_id", dbInstanceID, "state", next, "error", err)
		return false
	}

	s.logger.InfoContext(ctx, "reconcile: a DB instance changed state",
		"db_instance_id", dbInstanceID, "was_state", was, "state", next)
	return true
}

// serverBusy reports whether an unfinished task owns the VM behind a DB
// instance.
func (s *Service) serverBusy(ctx context.Context, instanceID string) (bool, error) {
	latest, err := s.store.GetLatestInstanceTask(ctx, sqlc.GetLatestInstanceTaskParams{
		ResourceID: sql.NullString{String: instanceID, Valid: true},
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	switch task.State(latest.State) {
	case task.StatePending, task.StateRunning:
		return true, nil
	default:
		return false, nil
	}
}

// probeDue reports whether this DB instance may be dialed again.
func (s *Service) probeDue(dbInstanceID string) bool {
	last, ok := s.probedAt.Load(dbInstanceID)
	if !ok {
		return true
	}
	at, ok := last.(time.Time)
	if !ok {
		return true
	}
	return s.now().Sub(at) >= s.probeInterval
}

// markProbed records that a DB instance was just dialed.
func (s *Service) markProbed(dbInstanceID string) {
	s.probedAt.Store(dbInstanceID, s.now())
}
