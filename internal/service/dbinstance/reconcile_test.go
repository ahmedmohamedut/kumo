package dbinstance_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/dbinstance"
)

// The table of section 6, one row at a time: what the pass does with a DB
// instance given what its VM is doing and whether the engine answers.
func TestReconcileTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setUp leaves the harness in the observed situation.
		setUp func(t *testing.T, h harness, created dbinstance.DBInstance)
		want  dbinstance.State
		// reason, when set, must appear in the recorded state_reason.
		reason string
		// advance is how far the clock moves before the pass. It has to move at
		// all, because the create already probed and one probe per interval is
		// the rule; how far matters for a transitional state, which is judged
		// against the readiness timeout. Zero means a second.
		advance time.Duration
	}{
		{
			name:  "available and answering stays available",
			setUp: func(*testing.T, harness, dbinstance.DBInstance) {},
			want:  dbinstance.StateAvailable,
		},
		{
			name: "available and silent fails",
			setUp: func(_ *testing.T, h harness, _ dbinstance.DBInstance) {
				h.probe.set(false)
			},
			want:   dbinstance.StateFailed,
			reason: "engine_not_responding",
		},
		{
			name: "failed and answering comes back",
			setUp: func(t *testing.T, h harness, created dbinstance.DBInstance) {
				h.forceState(t, created.ID, dbinstance.StateFailed)
			},
			want: dbinstance.StateAvailable,
		},
		{
			name: "a guest stopped out of band stops the database",
			setUp: func(t *testing.T, h harness, created dbinstance.DBInstance) {
				if err := h.hv.Destroy(t.Context(), created.InstanceID); err != nil {
					t.Fatalf("Destroy(): %v", err)
				}
				// The instance pass is what records the VM as stopped; this
				// one reads that result, so it runs first here as it does in
				// serve.go.
				if _, err := h.instances.Reconcile(t.Context()); err != nil {
					t.Fatalf("instances.Reconcile(): %v", err)
				}
			},
			want: dbinstance.StateStopped,
		},
		{
			name: "a guest whose domain is gone fails the database",
			setUp: func(t *testing.T, h harness, created dbinstance.DBInstance) {
				if err := h.hv.Destroy(t.Context(), created.InstanceID); err != nil {
					t.Fatalf("Destroy(): %v", err)
				}
				if err := h.hv.Undefine(t.Context(), created.InstanceID); err != nil {
					t.Fatalf("Undefine(): %v", err)
				}
				if _, err := h.instances.Reconcile(t.Context()); err != nil {
					t.Fatalf("instances.Reconcile(): %v", err)
				}
			},
			want:   dbinstance.StateFailed,
			reason: "server_failed",
		},
		{
			name: "a database still coming up is left alone",
			setUp: func(t *testing.T, h harness, created dbinstance.DBInstance) {
				h.probe.set(false)
				h.forceState(t, created.ID, dbinstance.StateStarting)
			},
			want: dbinstance.StateStarting,
			// Far enough for the probe to be due again, and well inside the
			// readiness timeout the transitional state is judged against.
			advance: 10 * time.Millisecond,
		},
		{
			name: "a database that never came up fails once the timeout passed",
			setUp: func(t *testing.T, h harness, created dbinstance.DBInstance) {
				h.probe.set(false)
				h.forceState(t, created.ID, dbinstance.StateStarting)
				// The transitional state is only a failure once it has had the
				// whole readiness timeout, which is measured on the row.
				h.advance(time.Hour)
			},
			want:   dbinstance.StateFailed,
			reason: "within",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			created := h.available(t, "orders")
			tc.setUp(t, h, created)

			advance := tc.advance
			if advance == 0 {
				advance = time.Second
			}
			h.advance(advance)
			if _, err := h.svc.Reconcile(t.Context()); err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}

			got := h.get(t, created.ID)
			if got.State != tc.want {
				t.Errorf("State = %q, want %q (reason %v)", got.State, tc.want, got.StateReason)
			}
			if tc.reason != "" {
				if got.StateReason == nil || !strings.Contains(*got.StateReason, tc.reason) {
					t.Errorf("StateReason = %v, want it to contain %q", got.StateReason, tc.reason)
				}
			}
		})
	}
}

// The recovery the whole arrangement exists for: a create whose engine was
// still coming up when the timeout ran out is not lost. The pass finds the
// server running, dials the engine, and publishes the database.
//
// It only works because the create recorded the VM as running on its way out:
// a `pending` row is skipped by both passes as "owned by a launch", and the DB
// instance would have stayed `failed` for ever.
func TestReconcileRecoversADBInstanceWhoseEngineCameUpLate(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.engineImage(t, "postgres-18", "18")
	created := h.create(t, "orders")

	h.probe.set(false)
	if err := h.runCreate(t, created.ID); err == nil {
		t.Fatal("CreateHandler() succeeded while the engine was silent")
	}
	if got := h.get(t, created.ID); got.State != dbinstance.StateFailed {
		t.Fatalf("State = %q, want failed", got.State)
	}
	if state := h.instanceState(t, created.InstanceID); state != "running" {
		t.Fatalf("the VM is %q, want running: a pending row is skipped by both passes", state)
	}

	// The engine finishes its first boot a moment later.
	h.probe.set(true)
	h.advance(time.Second)
	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	got := h.get(t, created.ID)
	if got.State != dbinstance.StateAvailable {
		t.Fatalf("State = %q (%v), want available", got.State, got.StateReason)
	}
	if got.StateReason != nil {
		t.Errorf("StateReason = %q, want it cleared", *got.StateReason)
	}
	// And the endpoint is published here, because the create never got that
	// far. The name already resolves -- the static DHCP host was written
	// during the build -- so a recovered database without one would be a
	// working database the console cannot offer a connection string for.
	if got.Endpoint == nil || *got.Endpoint != "orders.default" {
		t.Errorf("Endpoint = %v, want orders.default", got.Endpoint)
	}
}

// A pass never touches the guest, whatever it finds. The reconciler reports;
// the operator or a reboot acts.
func TestReconcileNeverTouchesTheGuest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")
	h.probe.set(false)
	h.advance(time.Second)

	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}

	info, err := h.hv.GetDomain(t.Context(), created.InstanceID)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	if info.State != hypervisor.DomainStateRunning {
		t.Errorf("the domain is %q; the pass stopped a guest it should only have reported", info.State)
	}
	if names := h.volumeNames(); len(names) != 3 {
		t.Errorf("the pass changed the pool: %v", names)
	}
}

// A creating or deleting DB instance belongs to its task, not to the pass.
func TestReconcileSkipsWhatATaskOwns(t *testing.T) {
	t.Parallel()

	for _, state := range []dbinstance.State{dbinstance.StateCreating, dbinstance.StateDeleting} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			created := h.available(t, "orders")
			h.forceState(t, created.ID, state)
			h.probe.set(false)
			h.advance(time.Second)

			report, err := h.svc.Reconcile(t.Context())
			if err != nil {
				t.Fatalf("Reconcile(): %v", err)
			}
			if report.Skipped != 1 || report.Updated != 0 {
				t.Errorf("report = %+v, want one skipped and nothing updated", report)
			}
			if got := h.get(t, created.ID); got.State != state {
				t.Errorf("State = %q, want %q left alone", got.State, state)
			}
		})
	}
}

// While a lifecycle task is in flight the transitional state is the truth and
// the guest is mid-change: judging it now would publish a database that is
// about to go down.
func TestReconcileSkipsADBInstanceWhoseVMIsBusy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")

	if _, err := h.svc.Stop(t.Context(), created.ID); err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	h.advance(time.Second)

	report, err := h.svc.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if report.Skipped != 1 {
		t.Errorf("report = %+v, want the DB instance skipped while its VM has a task", report)
	}
	if got := h.get(t, created.ID); got.State != dbinstance.StateStopping {
		t.Errorf("State = %q, want stopping left alone", got.State)
	}
}

// One probe per DB instance per interval, however often the pass runs: a node
// with no databases does no work, and a node with one does not dial it twice
// in a tick.
func TestReconcileProbesAtMostOncePerInterval(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")
	before := h.probe.count()

	for range 5 {
		if _, err := h.svc.Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile(): %v", err)
		}
	}
	if got := h.probe.count(); got != before {
		t.Errorf("the pass dialed %d times inside one interval, want none", got-before)
	}

	h.advance(time.Second)
	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if got := h.probe.count(); got != before+1 {
		t.Errorf("the pass dialed %d times in the next interval, want 1", got-before)
	}

	// And a node with nothing to probe does nothing at all.
	if _, err := h.svc.Delete(t.Context(), created.ID); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if err := h.runDelete(t, created.ID); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}
	h.advance(time.Second)
	dialed := h.probe.count()
	report, err := h.svc.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if report.Rows != 0 || h.probe.count() != dialed {
		t.Errorf("report = %+v and %d dials, want an empty pass", report, h.probe.count()-dialed)
	}
}

// The same reason twice must not churn updated_at on every pass: a row that
// already says what the pass found is left alone.
func TestReconcileDoesNotRewriteTheSameFailure(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.available(t, "orders")
	h.probe.set(false)
	h.advance(time.Second)

	if _, err := h.svc.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	first := h.row(t, created.ID)
	if first.State != string(dbinstance.StateFailed) {
		t.Fatalf("State = %q, want failed", first.State)
	}

	h.advance(time.Second)
	report, err := h.svc.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	if report.Updated != 0 {
		t.Errorf("report = %+v, want nothing updated the second time", report)
	}
	if second := h.row(t, created.ID); second.UpdatedAt != first.UpdatedAt {
		t.Errorf("updated_at moved from %q to %q with nothing to say",
			first.UpdatedAt, second.UpdatedAt)
	}
}
