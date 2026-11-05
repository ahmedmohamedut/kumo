package dbinstance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Stop asks the engine's guest to power off.
//
// It is an ACPI shutdown and never a destroy, so Postgres gets its shutdown
// checkpoint; the watchdog that bounds the wait is the instance service's
// (docs/design/07-databases.md section 4).
func (s *Service) Stop(ctx context.Context, dbInstanceID string) (DBInstance, error) {
	return s.delegate(ctx, dbInstanceID, dbAction{
		name:  "stop",
		from:  []State{StateAvailable},
		next:  StateStopping,
		queue: (*instance.Service).StopManaged,
	})
}

// Start boots a stopped DB instance. The address and the MAC are stable across
// a stop and a start by construction, so the endpoint does not move; the
// readiness probe is what republishes it as available (section 4).
func (s *Service) Start(ctx context.Context, dbInstanceID string) (DBInstance, error) {
	return s.delegate(ctx, dbInstanceID, dbAction{
		name:  "start",
		from:  []State{StateStopped},
		next:  StateStarting,
		queue: (*instance.Service).StartManaged,
	})
}

// Reboot restarts a running DB instance.
func (s *Service) Reboot(ctx context.Context, dbInstanceID string) (DBInstance, error) {
	return s.delegate(ctx, dbInstanceID, dbAction{
		name:  "reboot",
		from:  []State{StateAvailable},
		next:  StateRebooting,
		queue: (*instance.Service).RebootManaged,
	})
}

// dbAction is one lifecycle request that the instance service carries out: the
// states it may be asked from, the state the DB instance moves to while the VM
// works, and the instance action to queue.
type dbAction struct {
	name  string
	from  []State
	next  State
	queue func(*instance.Service, context.Context, string, instance.ManagedBy) (instance.Instance, error)
}

// allows reports whether the action may be asked for from state.
func (a dbAction) allows(state State) bool {
	for _, candidate := range a.from {
		if candidate == state {
			return true
		}
	}
	return false
}

// delegate runs a lifecycle action: it checks the DB instance's own state
// machine, hands the action to the instance service and records the
// transitional state.
//
// The VM is asked first and the row written second, on purpose. If the write
// fails the guest is still doing what was asked and the reconciler brings the
// row in line on its next pass (section 6); if the order were reversed, a
// refusal from the instance service would leave a DB instance claiming to be
// stopping when nothing is.
func (s *Service) delegate(ctx context.Context, dbInstanceID string, act dbAction) (DBInstance, error) {
	release, err := s.tryLock(dbInstanceID, act.name)
	if err != nil {
		return DBInstance{}, err
	}
	defer release()

	row, err := s.row(ctx, dbInstanceID)
	if err != nil {
		return DBInstance{}, err
	}
	if !act.allows(State(row.State)) {
		return DBInstance{}, fmt.Errorf("%w: the DB instance %q is %s, and %s only applies to a %s one",
			service.ErrConflict, row.ID, row.State, act.name, expects(act.from))
	}

	queued, err := act.queue(s.instances, ctx, row.InstanceID, instance.ManagedByDatabases)
	if err != nil {
		return DBInstance{}, err
	}

	updated, err := s.store.SetDBInstanceState(ctx, sqlc.SetDBInstanceStateParams{
		ID:          row.ID,
		State:       string(act.next),
		StateReason: sql.NullString{},
		UpdatedAt:   formatTime(s.now()),
	})
	if err != nil {
		// The guest is already being acted on, so this is not a failure worth
		// reporting as one: the reconcile pass writes the state the VM ends up
		// in either way.
		s.logger.ErrorContext(ctx, "dbinstance: the server is acting but the row was not updated",
			"db_instance_id", row.ID, "action", act.name, "error", err)
		return DBInstance{}, fmt.Errorf("mark DB instance %q %s: %w", row.ID, act.next, err)
	}

	out, err := s.Get(ctx, row.ID)
	if err != nil {
		return DBInstance{}, err
	}
	// The task just queued is the one the console follows. It belongs to the
	// VM rather than to the DB instance, which is why it is named here instead
	// of being read back from the DB instance's own tasks.
	out.TaskID = queued.TaskID

	s.logger.InfoContext(ctx, "dbinstance: lifecycle action queued",
		"db_instance_id", row.ID, "identifier", row.Identifier, "action", act.name,
		"state", updated.State, "instance_id", row.InstanceID, "task_id", deref(queued.TaskID))

	return out, nil
}

// Delete queues the removal of a DB instance: the guest, the data disk and both
// rows (docs/design/07-databases.md section 4).
//
// It is allowed from every state but `deleting`, because it is also the way out
// of one that is stuck -- a create whose engine never answered, or one whose
// credential was lost to a restart. The data is gone afterwards: there is no
// final snapshot and no recovery, which is what the console's confirmation says
// in as many words.
//
// The DB instance lock is deliberately not taken. A create that is waiting for
// an engine holds it for as long as the readiness timeout, and delete must not
// be the one action an operator cannot reach while something is stuck; the
// create task watches for `deleting` and stands down instead.
func (s *Service) Delete(ctx context.Context, dbInstanceID string) (DBInstance, error) {
	var taskID string

	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no DB instance %q", service.ErrNotFound, dbInstanceID)
			}
			return fmt.Errorf("get DB instance: %w", err)
		}
		if State(row.State) == StateDeleting {
			return fmt.Errorf("%w: the DB instance %q is already being deleted",
				service.ErrConflict, row.ID)
		}

		if _, err := tx.SetDBInstanceState(ctx, sqlc.SetDBInstanceStateParams{
			ID:          row.ID,
			State:       string(StateDeleting),
			StateReason: sql.NullString{},
			UpdatedAt:   formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark DB instance %q deleting: %w", row.ID, err)
		}

		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeDBInstanceDelete, row.ID,
			deletePayload{DBInstanceID: row.ID})
		if err != nil {
			return err
		}
		taskID = queued.ID
		return nil
	})
	if err != nil {
		return DBInstance{}, err
	}

	out, err := s.Get(ctx, dbInstanceID)
	if err != nil {
		return DBInstance{}, err
	}
	out.TaskID = &taskID

	s.logger.InfoContext(ctx, "dbinstance: deletion queued",
		"db_instance_id", out.ID, "identifier", out.Identifier, "task_id", taskID)

	return out, nil
}

// deletePayload is the task input of a delete.
type deletePayload struct {
	DBInstanceID string `json:"db_instance_id"`
}

// row reads a DB instance row, mapping a missing one onto a 404.
func (s *Service) row(ctx context.Context, dbInstanceID string) (sqlc.DbInstance, error) {
	row, err := s.store.GetDBInstance(ctx, sqlc.GetDBInstanceParams{ID: dbInstanceID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.DbInstance{}, fmt.Errorf("%w: no DB instance %q",
				service.ErrNotFound, dbInstanceID)
		}
		return sqlc.DbInstance{}, fmt.Errorf("get DB instance: %w", err)
	}
	return row, nil
}

// tryLock takes the DB instance's lock for the duration of a request, refusing
// rather than waiting: a request that arrives while a task is working on the
// same database is a conflict, not something to queue behind.
func (s *Service) tryLock(dbInstanceID, holder string) (func(), error) {
	release, err := s.locks.TryAcquire(LockKey(dbInstanceID), holder)
	if err != nil {
		current, held := s.locks.Holder(LockKey(dbInstanceID))
		if !held {
			current = "another operation"
		}
		return nil, fmt.Errorf("%w: the DB instance %q is busy with %s",
			service.ErrConflict, dbInstanceID, current)
	}
	return release, nil
}

// expects names the states an action can start from, for the conflict message.
func expects(states []State) string {
	names := make([]string, 0, len(states))
	for _, state := range states {
		names = append(names, string(state))
	}
	switch len(names) {
	case 0:
		return "different"
	case 1:
		return names[0]
	default:
		return names[0] + " or " + names[len(names)-1]
	}
}

// deref renders a nullable string for a log line.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
