package dbinstance_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// A stop asks the guest to power off and leaves the DB instance stopping until
// the VM is actually off; the reconcile pass is what writes stopped.
func TestStopDelegatesToTheVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")

	stopped, err := h.svc.Stop(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if stopped.State != dbinstance.StateStopping {
		t.Errorf("State = %q, want stopping", stopped.State)
	}
	// The task the console follows is the VM's, because that is where the work
	// is.
	if stopped.TaskID == nil {
		t.Fatal("TaskID is nil; the console has nothing to follow")
	}
	if state := h.instanceState(t, created.InstanceID); state != string(instance.StateStopping) {
		t.Errorf("the VM is %q, want stopping", state)
	}

	// The guest was asked rather than destroyed: a database gets its shutdown
	// checkpoint.
	if err := h.runInstanceTask(t, created.InstanceID); err != nil {
		t.Fatalf("the instance stop task: %v", err)
	}
	if state := h.instanceState(t, created.InstanceID); state != string(instance.StateStopped) {
		t.Errorf("the VM is %q, want stopped", state)
	}

	// The endpoint survives a stop: the address and the MAC are stable by
	// construction, so a client's connection string does not change.
	if got := h.get(t, created.ID); got.Endpoint == nil || *got.Endpoint != "orders.default" {
		t.Errorf("Endpoint = %v after a stop, want it unchanged", got.Endpoint)
	}
}

// Start boots the VM again and leaves the DB instance starting; the probe is
// what republishes it as available.
func TestStartDelegatesToTheVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")
	h.stopCompletely(t, created)

	started, err := h.svc.Start(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if started.State != dbinstance.StateStarting {
		t.Errorf("State = %q, want starting", started.State)
	}
	if err := h.runInstanceTask(t, created.InstanceID); err != nil {
		t.Fatalf("the instance start task: %v", err)
	}
	if state := h.instanceState(t, created.InstanceID); state != string(instance.StateRunning) {
		t.Errorf("the VM is %q, want running", state)
	}

	// Still starting until something has asked the engine.
	if got := h.get(t, created.ID); got.State != dbinstance.StateStarting {
		t.Errorf("State = %q, want starting until the probe answers", got.State)
	}
	// One probe per DB instance per interval, so the pass only dials again
	// once the clock has moved (section 6, "Rate").
	h.advance(time.Second)
	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if got := h.get(t, created.ID); got.State != dbinstance.StateAvailable {
		t.Errorf("State = %q, want available once the engine answered", got.State)
	}
}

// Reboot keeps the VM running and the DB instance rebooting until the engine
// answers again.
func TestRebootDelegatesToTheVM(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")

	rebooted, err := h.svc.Reboot(t.Context(), created.ID)
	if err != nil {
		t.Fatalf("Reboot(): %v", err)
	}
	if rebooted.State != dbinstance.StateRebooting {
		t.Errorf("State = %q, want rebooting", rebooted.State)
	}
	if err := h.runInstanceTask(t, created.InstanceID); err != nil {
		t.Fatalf("the instance reboot task: %v", err)
	}
	h.advance(time.Second)
	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if got := h.get(t, created.ID); got.State != dbinstance.StateAvailable {
		t.Errorf("State = %q, want available", got.State)
	}
}

// Every transition that does not apply is a conflict naming the state the DB
// instance is in, which is the state machine rather than a guess about what an
// operator probably meant.
func TestLifecycleRefusesTransitionsThatDoNotApply(t *testing.T) {
	t.Parallel()

	actions := map[string]func(h harness, id string) error{
		"stop": func(h harness, id string) error {
			_, err := h.svc.Stop(t.Context(), id)
			return err
		},
		"start": func(h harness, id string) error {
			_, err := h.svc.Start(t.Context(), id)
			return err
		},
		"reboot": func(h harness, id string) error {
			_, err := h.svc.Reboot(t.Context(), id)
			return err
		},
	}

	// The state a DB instance is in, and the actions that must be refused from
	// it. Everything not listed is the one action that applies.
	tests := []struct {
		state   dbinstance.State
		refused []string
	}{
		{dbinstance.StateCreating, []string{"stop", "start", "reboot"}},
		{dbinstance.StateAvailable, []string{"start"}},
		{dbinstance.StateStopped, []string{"stop", "reboot"}},
		{dbinstance.StateStarting, []string{"stop", "start", "reboot"}},
		{dbinstance.StateStopping, []string{"stop", "start", "reboot"}},
		{dbinstance.StateRebooting, []string{"stop", "start", "reboot"}},
		{dbinstance.StateFailed, []string{"stop", "start", "reboot"}},
		{dbinstance.StateDeleting, []string{"stop", "start", "reboot"}},
	}

	for _, tc := range tests {
		t.Run(string(tc.state), func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			created := h.available(t, "orders")
			h.forceState(t, created.ID, tc.state)

			for _, name := range tc.refused {
				err := actions[name](h, created.ID)
				if !errors.Is(err, service.ErrConflict) {
					t.Errorf("%s from %s: error = %v, want ErrConflict", name, tc.state, err)
				}
				if err != nil && !strings.Contains(err.Error(), string(tc.state)) {
					t.Errorf("%s from %s: error = %q, want it to name the state",
						name, tc.state, err)
				}
			}
		})
	}
}

// A lifecycle action on a DB instance that does not exist is a 404.
func TestLifecycleOnAnUnknownDBInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	for name, action := range map[string]func(string) error{
		"stop":   func(id string) error { _, err := h.svc.Stop(t.Context(), id); return err },
		"start":  func(id string) error { _, err := h.svc.Start(t.Context(), id); return err },
		"reboot": func(id string) error { _, err := h.svc.Reboot(t.Context(), id); return err },
		"delete": func(id string) error { _, err := h.svc.Delete(t.Context(), id); return err },
	} {
		if err := action("db-0a1b2c3d4e5f60718"); !errors.Is(err, service.ErrNotFound) {
			t.Errorf("%s: error = %v, want ErrNotFound", name, err)
		}
	}
}

// Delete is allowed from every state but deleting, because it is also the way
// out of one that is stuck.
func TestDeleteIsAllowedFromEveryStateButDeleting(t *testing.T) {
	t.Parallel()

	for _, state := range []dbinstance.State{
		dbinstance.StateCreating, dbinstance.StateAvailable, dbinstance.StateStarting,
		dbinstance.StateStopping, dbinstance.StateStopped, dbinstance.StateRebooting,
		dbinstance.StateFailed,
	} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			created := h.available(t, "orders")
			h.forceState(t, created.ID, state)

			deleting, err := h.svc.Delete(t.Context(), created.ID)
			if err != nil {
				t.Fatalf("Delete() from %s: %v", state, err)
			}
			if deleting.State != dbinstance.StateDeleting {
				t.Errorf("State = %q, want deleting", deleting.State)
			}
			if deleting.TaskID == nil {
				t.Error("TaskID is nil; the console has nothing to follow")
			}

			// And a second delete is a conflict rather than a second task.
			if _, err := h.svc.Delete(t.Context(), created.ID); !errors.Is(err, service.ErrConflict) {
				t.Errorf("the repeated Delete() error = %v, want ErrConflict", err)
			}
		})
	}
}

// The VM behind a DB instance is not reachable through the Instances API, and
// the refusal names the resource to use instead. This is the other half of the
// guard M7.3 added: the module itself may act on its own VM.
func TestTheBackingVMIsOnlyReachableThroughThisService(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")

	for name, action := range map[string]func() error{
		"stop":      func() error { _, err := h.instances.Stop(t.Context(), created.InstanceID, false); return err },
		"start":     func() error { _, err := h.instances.Start(t.Context(), created.InstanceID); return err },
		"reboot":    func() error { _, err := h.instances.Reboot(t.Context(), created.InstanceID, false); return err },
		"terminate": func() error { _, err := h.instances.Terminate(t.Context(), created.InstanceID); return err },
	} {
		err := action()
		if !errors.Is(err, service.ErrConflict) {
			t.Errorf("instances.%s: error = %v, want ErrConflict", name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "/rds/db-instances") {
			t.Errorf("instances.%s: error = %q, want it to name the path to use", name, err)
		}
	}

	// The module's own path works on the same row.
	if _, err := h.svc.Stop(t.Context(), created.ID); err != nil {
		t.Errorf("Stop(): %v", err)
	}
}

// stopCompletely stops a DB instance and runs the VM's task, leaving both rows
// in their stopped state.
func (h *harness) stopCompletely(t *testing.T, created dbinstance.DBInstance) {
	t.Helper()

	if _, err := h.svc.Stop(t.Context(), created.ID); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if err := h.runInstanceTask(t, created.InstanceID); err != nil {
		t.Fatalf("the instance stop task: %v", err)
	}
	h.forceState(t, created.ID, dbinstance.StateStopped)
}

// forceState writes a state onto a DB instance row, so a test can assert on a
// transition from a state that would otherwise take a whole workflow to reach.
func (h *harness) forceState(t *testing.T, dbInstanceID string, state dbinstance.State) {
	t.Helper()

	if _, err := h.store.SetDBInstanceState(t.Context(), sqlc.SetDBInstanceStateParams{
		ID:        dbInstanceID,
		State:     string(state),
		UpdatedAt: h.now.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}); err != nil {
		t.Fatalf("SetDBInstanceState(%q): %v", state, err)
	}
}
