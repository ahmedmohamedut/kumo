package instance_test

import (
	"database/sql"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// The reconciler is the safety net of docs/design/00-architecture.md §4: libvirt
// owns the runtime state and SQLite owns the metadata, so every test here sets
// the two at odds behind Kumo's back and asserts that a pass picks the
// hypervisor's answer.

// nullString is the sql.NullString a test writes a state_reason with.
func nullString(v string) sql.NullString {
	return sql.NullString{String: v, Valid: true}
}

// orphanOf finds one domain name in the orphan report.
func orphanOf(t *testing.T, svc *instance.Service, name string) (instance.Orphan, bool) {
	t.Helper()

	for _, o := range svc.Orphans() {
		if o.DomainName == name {
			return o, true
		}
	}
	return instance.Orphan{}, false
}

// reconcile runs one pass and fails the test if it could not complete.
func (h *harness) reconcile(t *testing.T) instance.Reconciliation {
	t.Helper()

	report, err := h.svc.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile(): %v", err)
	}
	return report
}

// TestReconcileAdoptsADomainStoppedOutOfBand is M4.5's acceptance criterion: a
// guest killed with `virsh destroy` is reflected in what the console reads.
func TestReconcileAdoptsADomainStoppedOutOfBand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-01")

	// What `virsh destroy` does: the domain is still defined, its guest is off,
	// and nothing told Kumo.
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}

	report := h.reconcile(t)
	if report.Updated != 1 {
		t.Errorf("Updated = %d, want 1", report.Updated)
	}

	got, reason := h.state(t, running.ID)
	if got != string(instance.StateStopped) {
		t.Errorf("state = %s, want stopped", got)
	}
	if reason != "" {
		t.Errorf("state_reason = %q, want empty: a stop is not a failure", reason)
	}
}

// TestReconcileAdoptsADomainStartedOutOfBand is the same discovery the other way
// round: `virsh start` on a stopped instance.
func TestReconcileAdoptsADomainStartedOutOfBand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web-02")

	if err := h.hv.Start(t.Context(), stopped.ID); err != nil {
		t.Fatalf("Start(): %v", err)
	}

	h.reconcile(t)

	got, _ := h.state(t, stopped.ID)
	if got != string(instance.StateRunning) {
		t.Errorf("state = %s, want running", got)
	}
}

// TestReconcileMarksAnInstanceWhoseDomainIsGone is rule 2 of §4: a row that is
// not transitional and has no domain can never be acted on again, so it moves to
// error with the reason the lifecycle workflows use for the same discovery.
func TestReconcileMarksAnInstanceWhoseDomainIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-03")

	// `virsh destroy` followed by `virsh undefine`: the domain is gone entirely.
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	h.reconcile(t)

	got, reason := h.state(t, running.ID)
	if got != string(instance.StateError) {
		t.Errorf("state = %s, want error", got)
	}
	if want := "domain_missing: "; len(reason) < len(want) || reason[:len(want)] != want {
		t.Errorf("state_reason = %q, want the domain_missing prefix", reason)
	}
}

// TestReconcileDoesNotRewriteAMissingDomain keeps the pass quiet. It runs every
// ten seconds, so a row it has already explained must not be written again: an
// updated_at that moves on every tick makes the console look busy and buries the
// moment the instance actually broke.
func TestReconcileDoesNotRewriteAMissingDomain(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-04")

	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	if first := h.reconcile(t); first.Updated != 1 {
		t.Fatalf("the first pass updated %d rows, want 1", first.Updated)
	}

	before, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	h.advance()

	if second := h.reconcile(t); second.Updated != 0 {
		t.Errorf("the second pass updated %d rows, want 0", second.Updated)
	}

	after, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved from %q to %q on a pass that changed nothing",
			before.UpdatedAt, after.UpdatedAt)
	}
}

// TestReconcileRecordsACrashedGuest covers the state no action of Kumo's can
// produce: qemu died under the guest.
func TestReconcileRecordsACrashedGuest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-05")

	if err := h.hv.Crash(t.Context(), running.ID); err != nil {
		t.Fatalf("Crash(): %v", err)
	}

	h.reconcile(t)

	got, reason := h.state(t, running.ID)
	if got != string(instance.StateError) {
		t.Errorf("state = %s, want error", got)
	}
	if want := "domain_crashed: "; len(reason) < len(want) || reason[:len(want)] != want {
		t.Errorf("state_reason = %q, want the domain_crashed prefix", reason)
	}
}

// TestReconcileRefreshesTheAddress is rule 4 of §4. The state already agrees
// here, so the pass has to look at the address even when it has no state change
// to make.
func TestReconcileRefreshesTheAddress(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-06")

	const renewed = "192.168.100.77"
	if running.PrivateIP != nil && *running.PrivateIP == renewed {
		t.Fatalf("the fixture already has the address this test renews into")
	}
	if err := h.hv.SetAddress(t.Context(), running.ID, renewed); err != nil {
		t.Fatalf("SetAddress(): %v", err)
	}

	report := h.reconcile(t)
	if report.Updated != 1 {
		t.Errorf("Updated = %d, want 1", report.Updated)
	}

	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if row.PrivateIp.String != renewed {
		t.Errorf("private_ip = %q, want %q", row.PrivateIp.String, renewed)
	}
	if instance.State(row.State) != instance.StateRunning {
		t.Errorf("state = %s, want running: an address change is not a state change", row.State)
	}
}

// TestReconcileNeverBlanksAnAddress protects the address an operator is using. A
// guest that is up but whose lease the hypervisor cannot report is still reachable
// at the address Kumo allocated to it, and forgetting it would strand the operator.
func TestReconcileNeverBlanksAnAddress(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-07")
	if running.PrivateIP == nil || *running.PrivateIP == "" {
		t.Fatal("the fixture instance has no address to lose")
	}
	allocated := *running.PrivateIP

	if err := h.hv.SetAddress(t.Context(), running.ID, ""); err != nil {
		t.Fatalf("SetAddress(): %v", err)
	}

	if report := h.reconcile(t); report.Updated != 0 {
		t.Errorf("Updated = %d, want 0", report.Updated)
	}

	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID})
	if err != nil {
		t.Fatalf("GetInstance(): %v", err)
	}
	if row.PrivateIp.String != allocated {
		t.Errorf("private_ip = %q, want the address the launch allocated (%q)",
			row.PrivateIp.String, allocated)
	}
}

// TestReconcileReportsAnOrphan is rule 3 of §4: a domain under an instance id
// that no row claims is reported and never removed.
func TestReconcileReportsAnOrphan(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-08")

	// The row goes while the domain stays, which is what a crash between the
	// define and the insert leaves behind.
	if _, err := h.store.DeleteInstance(t.Context(), sqlc.DeleteInstanceParams{ID: running.ID}); err != nil {
		t.Fatalf("DeleteInstance(): %v", err)
	}

	report := h.reconcile(t)
	if report.Orphans != 1 {
		t.Fatalf("Orphans = %d, want 1", report.Orphans)
	}

	orphan, ok := orphanOf(t, h.svc, running.ID)
	if !ok {
		t.Fatalf("Orphans() does not list %q", running.ID)
	}
	if orphan.State != instance.StateRunning {
		t.Errorf("orphan state = %s, want running", orphan.State)
	}
	if orphan.UUID == "" {
		t.Error("the orphan carries no uuid, which is what identifies it to virsh")
	}

	// The report says what is there; it must not have done anything about it.
	if _, err := h.hv.GetDomain(t.Context(), running.ID); err != nil {
		t.Errorf("the pass removed the orphaned domain: %v", err)
	}
}

// TestReconcileKeepsTheFirstSightingOfAnOrphan is what makes the report usable:
// an operator has to be able to tell a week-old leftover from one that appeared
// during the launch they are watching.
func TestReconcileKeepsTheFirstSightingOfAnOrphan(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-09")
	if _, err := h.store.DeleteInstance(t.Context(), sqlc.DeleteInstanceParams{ID: running.ID}); err != nil {
		t.Fatalf("DeleteInstance(): %v", err)
	}

	h.reconcile(t)
	first, ok := orphanOf(t, h.svc, running.ID)
	if !ok {
		t.Fatalf("the first pass does not list %q", running.ID)
	}

	h.advance()
	h.reconcile(t)
	second, ok := orphanOf(t, h.svc, running.ID)
	if !ok {
		t.Fatalf("the second pass does not list %q", running.ID)
	}

	if !second.FirstSeen.Equal(first.FirstSeen) {
		t.Errorf("FirstSeen moved from %s to %s", first.FirstSeen, second.FirstSeen)
	}
	if !second.LastSeen.After(first.LastSeen) {
		t.Errorf("LastSeen = %s, want it to move past %s", second.LastSeen, first.LastSeen)
	}
}

// TestReconcileForgetsAnOrphanThatIsGone keeps the report a description of now
// rather than a log of everything ever seen.
func TestReconcileForgetsAnOrphanThatIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-10")
	if _, err := h.store.DeleteInstance(t.Context(), sqlc.DeleteInstanceParams{ID: running.ID}); err != nil {
		t.Fatalf("DeleteInstance(): %v", err)
	}
	h.reconcile(t)
	if _, ok := orphanOf(t, h.svc, running.ID); !ok {
		t.Fatalf("the first pass does not list %q", running.ID)
	}

	// The operator cleaned it up by hand, which is what the report is for.
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	if report := h.reconcile(t); report.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0", report.Orphans)
	}
	if _, ok := orphanOf(t, h.svc, running.ID); ok {
		t.Errorf("Orphans() still lists %q after the domain went", running.ID)
	}
}

// TestReconcileIgnoresDomainsThatAreNotKumosS keeps the host's own guests out of
// the report. A node may run virtual machines nobody asked Kumo about, and
// calling those orphans would make the report noise an operator learns to skip.
func TestReconcileIgnoresDomainsThatAreNotKumos(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "handmade-image")

	const foreign = "my-own-vm"
	if err := h.hv.CreateRootDisk(t.Context(), hypervisor.RootDiskSpec{
		InstanceID:   foreign,
		SourceVolume: source.VolumeName,
		Pool:         instancesPool,
		SizeGiB:      1,
	}); err != nil {
		t.Fatalf("CreateRootDisk(): %v", err)
	}
	if _, err := h.hv.DefineAndStart(t.Context(), hypervisor.DomainSpec{
		ID:           foreign,
		Name:         foreign,
		VCPUs:        1,
		MemoryMiB:    512,
		RootDiskPool: instancesPool,
		Network:      h.network,
	}); err != nil {
		t.Fatalf("DefineAndStart(): %v", err)
	}

	report := h.reconcile(t)
	if report.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0: %q is not a Kumo domain", report.Orphans, foreign)
	}
	if report.Domains != 0 {
		t.Errorf("Domains = %d, want 0", report.Domains)
	}
}

// TestReconcileLeavesTransitionalStatesToTheirTask is §4's ownership rule. A
// pending row belongs to the launch that wrote it, and "no domain yet" is the
// expected reading rather than a fault to record.
func TestReconcileLeavesTransitionalStatesToTheirTask(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "pending-image")

	// Launched but the task never run: the row is pending and no domain exists.
	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: "web-11", ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(): %v", err)
	}

	report := h.reconcile(t)
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
	if report.Updated != 0 {
		t.Errorf("Updated = %d, want 0", report.Updated)
	}

	got, _ := h.state(t, launched.ID)
	if got != string(instance.StatePending) {
		t.Errorf("state = %s, want pending: the launch still owns this row", got)
	}
}

// TestReconcileSkipsAnInstanceATaskIsHolding keeps the pass out of a running
// workflow's way. A task holds the instance lock for as long as it works, and a
// pass that wrote the row underneath it would undo a decision made with more
// context than one ListDomains call.
func TestReconcileSkipsAnInstanceATaskIsHolding(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-12")

	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}

	release, err := h.locks.TryAcquire(instance.LockKey(running.ID), "instance.stop")
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}

	report := h.reconcile(t)
	if report.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", report.Skipped)
	}
	if got, _ := h.state(t, running.ID); got != string(instance.StateRunning) {
		t.Errorf("state = %s, want running: the pass wrote a row a task was holding", got)
	}

	// With the task done, the next pass sees what it saw all along.
	release()
	h.reconcile(t)
	if got, _ := h.state(t, running.ID); got != string(instance.StateStopped) {
		t.Errorf("state = %s, want stopped once the lock is free", got)
	}
}

// TestReconcileKeepsAFailedLaunchInError is the one case where libvirt's answer
// is not adopted wholesale. A launch that failed often leaves a domain defined
// but never started, and reading that as a clean "stopped" would turn a broken
// instance into one the console invites an operator to start.
func TestReconcileKeepsAFailedLaunchInError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-13")

	const reason = "launch failed: the seed iso could not be written"
	if _, err := h.store.SetInstanceState(t.Context(), sqlc.SetInstanceStateParams{
		ID:          running.ID,
		State:       string(instance.StateError),
		StateReason: nullString(reason),
		UpdatedAt:   "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SetInstanceState(): %v", err)
	}
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}

	if report := h.reconcile(t); report.Updated != 0 {
		t.Errorf("Updated = %d, want 0", report.Updated)
	}

	got, gotReason := h.state(t, running.ID)
	if got != string(instance.StateError) {
		t.Errorf("state = %s, want error", got)
	}
	if gotReason != reason {
		t.Errorf("state_reason = %q, want the original cause %q", gotReason, reason)
	}
}

// TestReconcileAdoptsAnErroredInstanceThatIsRunning is the other half of that
// rule: a guest that is demonstrably up is running, whatever an earlier verdict
// recorded. This is what lets an instance recover from a transient failure
// without an operator having to relaunch it.
func TestReconcileAdoptsAnErroredInstanceThatIsRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-14")

	if _, err := h.store.SetInstanceState(t.Context(), sqlc.SetInstanceStateParams{
		ID:          running.ID,
		State:       string(instance.StateError),
		StateReason: nullString("domain_missing: the domain of instance " + running.ID + " is gone"),
		UpdatedAt:   "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("SetInstanceState(): %v", err)
	}

	h.reconcile(t)

	got, reason := h.state(t, running.ID)
	if got != string(instance.StateRunning) {
		t.Errorf("state = %s, want running", got)
	}
	if reason != "" {
		t.Errorf("state_reason = %q, want it cleared with the recovery", reason)
	}
}

// TestReconcileInstanceActsOnOneEvent covers the event-driven path of
// docs/design/04-libvirt-integration.md §7: an event names a domain, so there is
// no reason to read the whole table to act on it.
func TestReconcileInstanceActsOnOneEvent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-15")
	other := h.runningInstance(t, "web-16")

	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Destroy(t.Context(), other.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}

	if err := h.svc.ReconcileInstance(t.Context(), running.ID); err != nil {
		t.Fatalf("ReconcileInstance(): %v", err)
	}

	if got, _ := h.state(t, running.ID); got != string(instance.StateStopped) {
		t.Errorf("the named instance is %s, want stopped", got)
	}
	if got, _ := h.state(t, other.ID); got != string(instance.StateRunning) {
		t.Errorf("the other instance is %s, want running: a hint names one domain", got)
	}
}

// TestReconcileInstanceToloratesAMissingRow is the normal end of a terminate: the
// undefine event arrives after the row is gone.
func TestReconcileInstanceToleratesAMissingRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if err := h.svc.ReconcileInstance(t.Context(), "i-00000000000000000"); err != nil {
		t.Errorf("ReconcileInstance() for an unknown instance: %v", err)
	}
}

// TestReconcileInstanceMarksAMissingDomain is the undefine event for a row that
// is still there, which is a domain somebody removed by hand.
func TestReconcileInstanceMarksAMissingDomain(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web-17")

	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	if err := h.svc.ReconcileInstance(t.Context(), running.ID); err != nil {
		t.Fatalf("ReconcileInstance(): %v", err)
	}

	if got, _ := h.state(t, running.ID); got != string(instance.StateError) {
		t.Errorf("state = %s, want error", got)
	}
}

// TestReconcileReportsWhatItSaw keeps the counters honest, since they are what an
// operator reads in the log to tell a quiet node from a reconciler that is not
// running.
func TestReconcileReportsWhatItSaw(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.runningInstance(t, "web-18")
	h.runningInstance(t, "web-19")

	report := h.reconcile(t)
	if report.Rows != 2 {
		t.Errorf("Rows = %d, want 2", report.Rows)
	}
	if report.Domains != 2 {
		t.Errorf("Domains = %d, want 2", report.Domains)
	}
	if report.Updated != 0 {
		t.Errorf("Updated = %d, want 0: nothing drifted", report.Updated)
	}
	if report.Orphans != 0 {
		t.Errorf("Orphans = %d, want 0", report.Orphans)
	}
}
