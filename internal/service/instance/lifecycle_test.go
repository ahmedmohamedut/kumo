package instance_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// runningInstance launches an instance and runs its launch task, so a lifecycle
// test starts from a domain that is really up.
func (h *harness) runningInstance(t *testing.T, name string) instance.Instance {
	t.Helper()

	source := h.availableImage(t, name+"-image")
	launched, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: name, ImageID: source.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch(%q): %v", name, err)
	}
	if err := h.launch(t, launched.ID); err != nil {
		t.Fatalf("LaunchHandler(%q): %v", name, err)
	}

	running, err := h.svc.Get(t.Context(), launched.ID, false)
	if err != nil {
		t.Fatalf("Get(%q): %v", name, err)
	}
	if running.State != instance.StateRunning {
		t.Fatalf("the fixture instance %q is %s, want running", name, running.State)
	}
	return running
}

// stoppedInstance is a runningInstance that has been through a graceful stop, so a
// start test starts from a domain that is really off.
func (h *harness) stoppedInstance(t *testing.T, name string) instance.Instance {
	t.Helper()

	running := h.runningInstance(t, name)
	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler(): %v", err)
	}

	stopped, err := h.svc.Get(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if stopped.State != instance.StateStopped {
		t.Fatalf("the fixture instance %q is %s, want stopped", name, stopped.State)
	}
	return stopped
}

// runTask runs the task of a queued lifecycle action the way the worker does, with
// the payload the service stored rather than one the test writes: a workflow that
// works from a payload the test made up is a test of the wrong thing.
func (h *harness) runTask(t *testing.T, queued instance.Instance, taskType string) error {
	t.Helper()

	work, row := h.pendingTask(t, queued, taskType)
	handle, ok := h.svc.Handler()[taskType]
	if !ok {
		t.Fatalf("the service registers no handler for %q", taskType)
	}
	err := handle.Handle(t.Context(), work)
	// The pool records the outcome after the handler returns, and a test that
	// leaves the task pending would have every later action refused as
	// "a task in progress" for the wrong reason.
	h.finishTask(t, row, err)
	return err
}

// pendingTask reads the task row a queued action created, so a workflow under test
// reads the payload the service stored rather than one the test wrote: a workflow
// that works from a payload the test made up is a test of the wrong thing.
func (h *harness) pendingTask(
	t *testing.T, queued instance.Instance, taskType string,
) (worker.Task, sqlc.Task) {
	t.Helper()

	if queued.TaskID == nil {
		t.Fatalf("%s queued no task for %q", taskType, queued.ID)
	}
	row, err := h.store.GetTask(t.Context(), sqlc.GetTaskParams{ID: *queued.TaskID})
	if err != nil {
		t.Fatalf("GetTask(%q): %v", *queued.TaskID, err)
	}
	if row.Type != taskType {
		t.Fatalf("the task is a %q, want %q", row.Type, taskType)
	}
	if row.State != string(task.StatePending) {
		t.Fatalf("the task is %q, want pending", row.State)
	}

	return worker.Task{
		ID:         row.ID,
		Type:       row.Type,
		ResourceID: row.ResourceID.String,
		Payload:    json.RawMessage(row.Payload),
	}, row
}

// finishTask is the bookkeeping worker.Pool does, so a task run by hand ends up
// in the state a task the pool ran would be in.
func (h *harness) finishTask(t *testing.T, row sqlc.Task, cause error) {
	t.Helper()

	params := sqlc.UpdateTaskParams{
		ID:        row.ID,
		State:     string(task.StateSucceeded),
		Progress:  100,
		Attempts:  row.Attempts + 1,
		UpdatedAt: h.now.UTC().Format(time.RFC3339Nano),
	}
	if cause != nil {
		params.State = string(task.StateFailed)
		params.Progress = row.Progress
		params.Error = sql.NullString{String: cause.Error(), Valid: true}
	}
	if _, err := h.store.UpdateTask(context.WithoutCancel(t.Context()), params); err != nil {
		t.Fatalf("UpdateTask(%q): %v", row.ID, err)
	}
}

// state reads the state column of an instance, so a test asserts on the row rather
// than on the projection the same service built.
func (h *harness) state(t *testing.T, instanceID string) (string, string) {
	t.Helper()

	row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		t.Fatalf("GetInstance(%q): %v", instanceID, err)
	}
	return row.State, row.StateReason.String
}

// setState writes a state behind the service's back, for the paths a test cannot
// reach through the API: an instance whose domain somebody removed, or a launch
// that overtook a terminate.
func (h *harness) setState(t *testing.T, instanceID string, state instance.State) {
	t.Helper()

	if _, err := h.store.SetInstanceState(t.Context(), sqlc.SetInstanceStateParams{
		ID:          instanceID,
		State:       string(state),
		StateReason: sql.NullString{},
		UpdatedAt:   h.now.UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("SetInstanceState(%q, %s): %v", instanceID, state, err)
	}
}

func TestStartBootsAStoppedInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web")

	queued, err := h.svc.Start(t.Context(), stopped.ID)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}

	// A start has no transitional state of its own: the row keeps saying stopped
	// until the guest is up, because pending there means a launch.
	if queued.State != instance.StateStopped {
		t.Fatalf("state = %q, want stopped: a start is not a launch", queued.State)
	}
	if queued.TaskID == nil {
		t.Fatal("Start() queued no task")
	}

	if err := h.runTask(t, queued, task.TypeInstanceStart); err != nil {
		t.Fatalf("StartHandler(): %v", err)
	}

	state, reason := h.state(t, stopped.ID)
	if state != string(instance.StateRunning) {
		t.Fatalf("state = %q with reason %q, want running", state, reason)
	}

	info, err := h.hv.GetDomain(t.Context(), stopped.ID)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	if info.State != hypervisor.DomainStateRunning {
		t.Fatalf("the domain is %s, want running", info.State)
	}

	// The address the row already had survives the boot, so a stopped instance
	// that starts again is reachable at the same place.
	got, err := h.svc.Get(t.Context(), stopped.ID, false)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.PrivateIP == nil || *got.PrivateIP == "" {
		t.Fatal("private_ip is empty after the start")
	}

	// Both the MAC and the static host that serves it are the launch
	// allocation: stopping and starting must not renumber the guest.
	if stopped.MACAddress == nil || got.MACAddress == nil || *got.MACAddress != *stopped.MACAddress {
		t.Fatalf("mac_address = %v, want the launch allocation %v", got.MACAddress, stopped.MACAddress)
	}
	if *got.PrivateIP != *stopped.PrivateIP {
		t.Fatalf("private_ip = %q after the start, want %q", *got.PrivateIP, *stopped.PrivateIP)
	}
	hosts := h.hv.DHCPHosts(h.network)
	if len(hosts) != 1 || hosts[0].MAC != *stopped.MACAddress || hosts[0].IP != *stopped.PrivateIP {
		t.Fatalf("dhcp hosts = %+v, want the launch allocation %s / %s still served",
			hosts, *stopped.MACAddress, *stopped.PrivateIP)
	}
}

func TestStartAdoptsADomainSomebodyStartedOutOfBand(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web")

	// Somebody started the guest with virsh while the row still said stopped.
	if err := h.hv.Start(t.Context(), stopped.ID); err != nil {
		t.Fatalf("fake Start(): %v", err)
	}

	queued, err := h.svc.Start(t.Context(), stopped.ID)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStart); err != nil {
		t.Fatalf("StartHandler() on a running domain: %v", err)
	}

	// libvirt refuses to boot a running domain, but the caller wanted exactly that
	// state, so the task adopts it instead of reporting a failure.
	if state, reason := h.state(t, stopped.ID); state != string(instance.StateRunning) {
		t.Fatalf("state = %q with reason %q, want running", state, reason)
	}
}

func TestStartFailsWhenTheDomainIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web")

	if err := h.hv.Undefine(t.Context(), stopped.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	queued, err := h.svc.Start(t.Context(), stopped.ID)
	if err != nil {
		t.Fatalf("Start(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStart); err == nil {
		t.Fatal("StartHandler() on a missing domain = nil, want an error")
	}

	// An instance whose domain libvirt does not know can never boot again, which is
	// what the reconciler concludes as well.
	state, reason := h.state(t, stopped.ID)
	if state != string(instance.StateError) {
		t.Fatalf("state = %q with reason %q, want error", state, reason)
	}
	if !strings.Contains(reason, "domain_missing") {
		t.Errorf("state_reason = %q, want it to name domain_missing", reason)
	}
}

func TestStopAsksTheGuestAndWaitsForIt(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}

	// The row moves to stopping in the transaction that creates the task, so the
	// console follows the request before the worker has done anything.
	if queued.State != instance.StateStopping {
		t.Fatalf("state = %q, want stopping", queued.State)
	}

	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler(): %v", err)
	}

	if state, reason := h.state(t, running.ID); state != string(instance.StateStopped) {
		t.Fatalf("state = %q with reason %q, want stopped", state, reason)
	}

	info, err := h.hv.GetDomain(t.Context(), running.ID)
	if err != nil {
		t.Fatalf("GetDomain(): %v", err)
	}
	if info.State != hypervisor.DomainStateShutoff {
		t.Fatalf("the domain is %s, want shutoff", info.State)
	}

	// The guest was asked, not overridden: a forced stop is the one that does not
	// wait, and the console says so.
	console, err := h.hv.ConsoleOutput(t.Context(), running.ID, 1<<16)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if !strings.Contains(string(console), "poweroff requested by ACPI") {
		t.Fatalf("the console does not mention the ACPI request:\n%s", console)
	}
	if strings.Contains(string(console), "power forced off") {
		t.Errorf("a graceful stop forced the guest off:\n%s", console)
	}
}

func TestForcedStopDoesNotWaitForTheGuest(t *testing.T) {
	t.Parallel()

	// A guest that never answers its ACPI request: the graceful path has to give
	// up, and the forced one must not notice.
	h := newHarnessWith(t, fake.Config{IgnoreShutdown: true}, instance.Options{
		StopTimeout: 30 * time.Millisecond,
	})
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Stop(t.Context(), running.ID, true)
	if err != nil {
		t.Fatalf("Stop(force): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler(force): %v", err)
	}

	if state, _ := h.state(t, running.ID); state != string(instance.StateStopped) {
		t.Fatalf("state = %q, want stopped", state)
	}
	console, err := h.hv.ConsoleOutput(t.Context(), running.ID, 1<<16)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if !strings.Contains(string(console), "power forced off") {
		t.Fatalf("a forced stop did not override the guest:\n%s", console)
	}
}

func TestStopGivesUpOnAGuestThatIgnoresTheRequest(t *testing.T) {
	t.Parallel()

	h := newHarnessWith(t, fake.Config{IgnoreShutdown: true}, instance.Options{
		StopTimeout: 30 * time.Millisecond,
	})
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}

	// The guest is not going to power off, so the watchdog turns the wait into an
	// error the operator can act on instead of an instance stuck in stopping.
	if err := h.runTask(t, queued, task.TypeInstanceStop); !errors.Is(err, instance.ErrNoShutdown) {
		t.Fatalf("StopHandler() error = %v, want ErrNoShutdown", err)
	}

	state, _ := h.state(t, running.ID)
	if state != string(instance.StateStopping) {
		t.Fatalf("state = %q, want stopping: the guest is still up", state)
	}

	// The way out of stopping is another stop, which is why stopping is a state the
	// stop may start from.
	forced, err := h.svc.Stop(t.Context(), running.ID, true)
	if err != nil {
		t.Fatalf("Stop(force) from stopping: %v", err)
	}
	if err := h.runTask(t, forced, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler(force): %v", err)
	}
	if state, _ := h.state(t, running.ID); state != string(instance.StateStopped) {
		t.Fatalf("state = %q, want stopped", state)
	}
}

func TestStopOfAnInstanceThatIsAlreadyOffIsRefused(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web")

	// A stop asks for something that is already true, so it is a conflict rather
	// than a task that would find nothing to do.
	_, err := h.svc.Stop(t.Context(), stopped.ID, false)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Stop() on a stopped instance = %v, want ErrConflict", err)
	}
}

func TestRebootKeepsTheInstanceRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Reboot(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Reboot(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceReboot); err != nil {
		t.Fatalf("RebootHandler(): %v", err)
	}

	// The instance was running and is running again, so the row does not move.
	if state, reason := h.state(t, running.ID); state != string(instance.StateRunning) {
		t.Fatalf("state = %q with reason %q, want running", state, reason)
	}
	console, err := h.hv.ConsoleOutput(t.Context(), running.ID, 1<<16)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if !strings.Contains(string(console), "guest reboot requested") {
		t.Fatalf("the console does not mention the reboot:\n%s", console)
	}
	if strings.Contains(string(console), "domain reset") {
		t.Errorf("a graceful reboot reset the domain:\n%s", console)
	}
}

func TestForcedRebootResetsTheDomain(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Reboot(t.Context(), running.ID, true)
	if err != nil {
		t.Fatalf("Reboot(force): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceReboot); err != nil {
		t.Fatalf("RebootHandler(force): %v", err)
	}

	console, err := h.hv.ConsoleOutput(t.Context(), running.ID, 1<<16)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if !strings.Contains(string(console), "domain reset") {
		t.Fatalf("a forced reboot did not reset the domain:\n%s", console)
	}
}

func TestRebootRecordsThatTheGuestIsNotRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// Somebody stopped the guest out of band. The row still says running, and the
	// reboot is the workflow that finds out: it cannot reboot, and the row must
	// stop claiming otherwise.
	if err := h.hv.Shutdown(t.Context(), running.ID); err != nil {
		t.Fatalf("fake Shutdown(): %v", err)
	}

	queued, err := h.svc.Reboot(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Reboot(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceReboot); err == nil {
		t.Fatal("RebootHandler() on a stopped domain = nil, want an error")
	}

	if state, _ := h.state(t, running.ID); state != string(instance.StateStopped) {
		t.Fatalf("state = %q, want stopped", state)
	}
}

func TestTerminateRemovesTheDomainAndTheVolumes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")
	source := h.runningImageOf(t, running.ID)

	// Tags and the console log are the two things an instance owns that are
	// neither its domain nor its volumes, and a terminate takes them too.
	if _, err := h.svc.Update(t.Context(), running.ID, instance.UpdateInput{
		Tags: map[string]string{"env": "test"},
	}, false); err != nil {
		t.Fatalf("Update() with tags: %v", err)
	}
	consoleLog := filepath.Join(h.console, running.ID+".log")
	if err := os.WriteFile(consoleLog, []byte("cloud-init: done\n"), 0o600); err != nil {
		t.Fatalf("write the console log: %v", err)
	}

	queued, err := h.svc.Terminate(t.Context(), running.ID)
	if err != nil {
		t.Fatalf("Terminate(): %v", err)
	}
	if queued.State != instance.StateTerminating {
		t.Fatalf("state = %q, want terminating", queued.State)
	}

	if err := h.runTask(t, queued, task.TypeInstanceTerminate); err != nil {
		t.Fatalf("TerminateHandler(): %v", err)
	}

	if _, err := h.hv.GetDomain(t.Context(), running.ID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Fatalf("GetDomain() after terminate = %v, want ErrNotFound", err)
	}
	for _, name := range []string{
		hypervisor.RootDiskName(running.ID),
		hypervisor.SeedISOName(running.ID),
	} {
		if contains(h.volumeNames(instancesPool), name) {
			t.Fatalf("the pool still holds %q: %v", name, h.volumeNames(instancesPool))
		}
	}
	// The static dhcp host went with the domain: a terminated instance takes no
	// leases, and its address belongs to the subnet again.
	if hosts := h.hv.DHCPHosts(h.network); len(hosts) != 0 {
		t.Fatalf("the network still holds the dhcp hosts %+v after terminate", hosts)
	}
	// The image volume is the backing file of other instances' disks and of every
	// launch to come, so a terminate must not touch it.
	if !contains(h.volumeNames(imagesPool), source) {
		t.Fatalf("the images pool lost %q: %v", source, h.volumeNames(imagesPool))
	}

	// The row is gone: a terminated instance is a deleted resource, not a record
	// of one (01-api.md §Instances). Everything that existed only because the row
	// did goes with it in the same transaction or the same cascade.
	if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstance() after terminate = %v, want sql.ErrNoRows", err)
	}
	if _, err := h.svc.Get(t.Context(), running.ID, false); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() after terminate = %v, want ErrNotFound", err)
	}
	interfaces, err := h.store.ListInterfacesByInstances(t.Context(), sqlc.ListInterfacesByInstancesParams{
		InstanceIds: []string{running.ID},
	})
	if err != nil {
		t.Fatalf("ListInterfacesByInstances(): %v", err)
	}
	if len(interfaces) != 0 {
		t.Errorf("network_interfaces = %+v, want none for a removed instance", interfaces)
	}
	tags, err := h.store.ListResourceTags(t.Context(), sqlc.ListResourceTagsParams{
		ResourceID: running.ID,
	})
	if err != nil {
		t.Fatalf("ListResourceTags(): %v", err)
	}
	if len(tags) != 0 {
		t.Errorf("tags = %+v, want none for a removed instance", tags)
	}
	if _, err := os.Stat(consoleLog); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the console log is still there: %v", err)
	}
	// The tasks stay: they are the record of what was done to the instance, and
	// the console follows the task that is doing the removing right now.
	if _, err := h.store.GetTask(t.Context(), sqlc.GetTaskParams{ID: *queued.TaskID}); err != nil {
		t.Errorf("GetTask() after terminate = %v, want the task kept", err)
	}

	// The name is free again, which is what the partial unique index on the name
	// is for — and with the row deleted there is nothing left to collide with.
	other := h.availableImage(t, "second-image")
	again, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: running.Name, ImageID: other.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch() reusing a terminated name: %v", err)
	}
	if again.ID == running.ID {
		t.Error("the new instance reuses the terminated id")
	}
}

// TestTerminateRemovesAStoppedInstance covers the state most instances sit in
// between runs: the domain is not running, and the hard stop that opens a
// terminate has to treat that as done rather than fail on the guest's state.
func TestTerminateRemovesAStoppedInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	stopped := h.stoppedInstance(t, "web")

	queued, err := h.svc.Terminate(t.Context(), stopped.ID)
	if err != nil {
		t.Fatalf("Terminate() of a stopped instance: %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceTerminate); err != nil {
		t.Fatalf("TerminateHandler(): %v", err)
	}

	if _, err := h.svc.Get(t.Context(), stopped.ID, false); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() after terminate = %v, want ErrNotFound", err)
	}
	for _, name := range []string{
		hypervisor.RootDiskName(stopped.ID),
		hypervisor.SeedISOName(stopped.ID),
	} {
		if contains(h.volumeNames(instancesPool), name) {
			t.Errorf("the pool still holds %q after terminating a stopped instance", name)
		}
	}
}

func TestTerminateIsSafeToRepeat(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Terminate(t.Context(), running.ID)
	if err != nil {
		t.Fatalf("Terminate(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceTerminate); err != nil {
		t.Fatalf("TerminateHandler(): %v", err)
	}

	// There is no row left to terminate again, so a repeat is a 404 rather than a
	// second task for a domain that is already gone. The client that never saw
	// the first answer learns the same thing from it: the instance is gone.
	if _, err := h.svc.Terminate(t.Context(), running.ID); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("Terminate() once the instance is gone = %v, want ErrNotFound", err)
	}
}

func TestTerminateFinishesAfterTheDomainIsAlreadyGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// An operator removed the domain by hand. The terminate still has volumes to
	// delete and a row to record, so it has to run rather than stop at the first
	// step.
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	queued, err := h.svc.Terminate(t.Context(), running.ID)
	if err != nil {
		t.Fatalf("Terminate(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceTerminate); err != nil {
		t.Fatalf("TerminateHandler(): %v", err)
	}

	// The row is the record being removed, and there was nothing left to do but
	// remove it: the volumes and the domain were already gone.
	if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstance() after terminate = %v, want sql.ErrNoRows", err)
	}
	if names := h.volumeNames(instancesPool); len(names) != 0 {
		t.Fatalf("the pool still holds %v", names)
	}
}

func TestTerminatePurgesARowAnEarlierReleaseLeftBehind(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// A release that kept the row left the database with rows in `terminated`:
	// domain and volumes gone, interface rows deleted, terminated_at set. Those
	// rows are still listed as history after an upgrade, and DELETE is what
	// removes them now rather than a migration that deletes history on its own.
	if _, err := h.store.Exec(t.Context(),
		"DELETE FROM network_interfaces WHERE instance_id = ?", running.ID); err != nil {
		t.Fatalf("delete the interface rows: %v", err)
	}
	h.setState(t, running.ID, instance.StateTerminated)

	queued, err := h.svc.Terminate(t.Context(), running.ID)
	if err != nil {
		t.Fatalf("Terminate() on a terminated row: %v", err)
	}
	if queued.State != instance.StateTerminating {
		t.Fatalf("state = %q, want terminating", queued.State)
	}
	if err := h.runTask(t, queued, task.TypeInstanceTerminate); err != nil {
		t.Fatalf("TerminateHandler(): %v", err)
	}

	if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: running.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetInstance() = %v, want sql.ErrNoRows", err)
	}

	// The name the row held is free the moment the row is gone, which is the
	// same promise the partial unique index used to make.
	other := h.availableImage(t, "second-image")
	again, err := h.svc.Launch(t.Context(), instance.LaunchInput{
		Name: running.Name, ImageID: other.ID, InstanceType: instanceType, RootDiskGB: 8,
	})
	if err != nil {
		t.Fatalf("Launch() reusing the name of a purged row: %v", err)
	}
	if again.ID == running.ID {
		t.Error("the new instance reuses the purged id")
	}
}

func TestOnlyTerminateTouchesATerminatedRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// A stop queued against what turns out to be a terminated row (one an
	// earlier release kept) has no domain to shut down: the task is done rather
	// than failed, because there is nothing left for it to do. Only a terminate
	// still has work against such a row — removing it.
	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	h.setState(t, running.ID, instance.StateTerminated)

	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler() on a terminated row: %v", err)
	}
	if state, _ := h.state(t, running.ID); state != string(instance.StateTerminated) {
		t.Errorf("state = %q, want the terminated row left alone", state)
	}
	if info, err := h.hv.GetDomain(t.Context(), running.ID); err != nil || info.State != hypervisor.DomainStateRunning {
		t.Errorf("GetDomain() = %+v, %v, want the domain untouched", info, err)
	}
}

func TestTerminateIsRefusedWhileAnotherTaskIsUnfinished(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// The stop is accepted and the task is still queued, so the row has not moved
	// yet. A terminate in this window would have to run behind a shutdown of a
	// domain it is about to delete.
	stopped, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if stopped.State != instance.StateStopping {
		t.Fatalf("state = %q, want stopping", stopped.State)
	}

	if _, err := h.svc.Terminate(t.Context(), running.ID); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Terminate() while a stop is queued = %v, want ErrConflict", err)
	}
	if _, err := h.svc.Start(t.Context(), running.ID); !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Start() while a stop is queued = %v, want ErrConflict", err)
	}
}

func TestActionsAreRefusedWhileTheInstanceIsLocked(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// A workflow holds the instance for as long as it works on it, and a request
	// that arrives in that window is a conflict rather than a second task to run
	// behind the first.
	release, err := h.locks.TryAcquire(instance.LockKey(running.ID), task.TypeInstanceLaunch)
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}

	for _, call := range []struct {
		action string
		do     func() error
	}{
		{"start", func() error { _, err := h.svc.Start(t.Context(), running.ID); return err }},
		{"stop", func() error { _, err := h.svc.Stop(t.Context(), running.ID, false); return err }},
		{"reboot", func() error { _, err := h.svc.Reboot(t.Context(), running.ID, false); return err }},
		{"terminate", func() error { _, err := h.svc.Terminate(t.Context(), running.ID); return err }},
	} {
		if err := call.do(); !errors.Is(err, service.ErrConflict) {
			t.Errorf("%s() on a locked instance = %v, want ErrConflict", call.action, err)
		}
	}
	// The message names what holds the instance, so the console can say what to
	// wait for instead of only that something is busy.
	_, err = h.svc.Reboot(t.Context(), running.ID, false)
	if !strings.Contains(err.Error(), task.TypeInstanceLaunch) {
		t.Errorf("the conflict message = %q, want it to name %s", err, task.TypeInstanceLaunch)
	}
	release()

	// The worker is the other half of the same rule: a workflow waits for the lock
	// rather than failing, because two tasks for one instance are going to run in
	// some order and the queue will run them both.
	queued, err := h.svc.Reboot(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Reboot(): %v", err)
	}
	work, row := h.pendingTask(t, queued, task.TypeInstanceReboot)

	held, err := h.locks.TryAcquire(instance.LockKey(running.ID), task.TypeInstanceLaunch)
	if err != nil {
		t.Fatalf("TryAcquire(): %v", err)
	}

	handle, ok := h.svc.Handler()[task.TypeInstanceReboot]
	if !ok {
		t.Fatalf("the service registers no handler for %q", task.TypeInstanceReboot)
	}

	done := make(chan error, 1)
	go func() { done <- handle.Handle(context.Background(), work) }()

	select {
	case err := <-done:
		t.Fatalf("RebootHandler() returned %v while the instance was locked", err)
	case <-time.After(50 * time.Millisecond):
	}
	held()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RebootHandler() after the release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RebootHandler() did not return after the lock was released")
	}
	h.finishTask(t, row, nil)
}

func TestActionsRefuseAnUnknownInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	for name, call := range map[string]func(context.Context, string) (instance.Instance, error){
		"start":     h.svc.Start,
		"stop":      func(ctx context.Context, id string) (instance.Instance, error) { return h.svc.Stop(ctx, id, false) },
		"reboot":    func(ctx context.Context, id string) (instance.Instance, error) { return h.svc.Reboot(ctx, id, false) },
		"terminate": h.svc.Terminate,
	} {
		if _, err := call(t.Context(), "i-0123456789abcdef0"); !errors.Is(err, service.ErrNotFound) {
			t.Errorf("%s() on an unknown instance = %v, want ErrNotFound", name, err)
		}
	}
}

func TestStopOfAnInstanceWithoutADomainRecordsItAsStopped(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// Somebody removed the domain by hand. A stop of a domain that is not there is
	// the outcome the caller asked for, so the instance is stopped rather than
	// failed: the row keeps claiming to be running and nobody can stop that claim
	// with an action, since an action may not start from running.
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}

	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler(): %v", err)
	}

	if state, _ := h.state(t, running.ID); state != string(instance.StateStopped) {
		t.Fatalf("state = %q, want stopped", state)
	}
	// And the way forward works: the instance starts again from the definition the
	// launch wrote, or fails on it, but it is no longer wedged in running.
	if _, err := h.svc.Start(t.Context(), running.ID); err != nil {
		t.Fatalf("Start() after a stop of a missing domain: %v", err)
	}
}

func TestLifecycleWorkflowsSkipAnInstanceThatIsGone(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	queued, err := h.svc.Stop(t.Context(), running.ID, false)
	if err != nil {
		t.Fatalf("Stop(): %v", err)
	}

	// The row is removed under the workflow, which is what a migration or a
	// database reset looks like from here. There is nothing to act on and nothing
	// to clean up, so the task is done rather than failed.
	if _, err := h.store.Exec(t.Context(), "DELETE FROM instances WHERE id = ?", running.ID); err != nil {
		t.Fatalf("delete the instance: %v", err)
	}
	if err := h.runTask(t, queued, task.TypeInstanceStop); err != nil {
		t.Fatalf("StopHandler() on a missing row: %v", err)
	}
}

func TestALaunchDoesNotResurrectATerminatingInstance(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// A terminate overtook the launch task: the row is going away, the domain is
	// gone and the volumes are being deleted. A launch that ran anyway would
	// rebuild what the terminate is removing, and end with a running guest for an
	// instance nobody asked for any more.
	h.setState(t, running.ID, instance.StateTerminating)
	if err := h.hv.Destroy(t.Context(), running.ID); err != nil {
		t.Fatalf("Destroy(): %v", err)
	}
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}
	// Undefine has just taken the domain's volumes with it (04-libvirt §3); the
	// explicit pass is what a terminate whose domain was already gone still runs,
	// so it tolerates volumes that are already deleted.
	for _, name := range []string{
		hypervisor.RootDiskName(running.ID),
		hypervisor.SeedISOName(running.ID),
	} {
		if err := h.hv.DeleteVolume(t.Context(), instancesPool, name); err != nil &&
			!errors.Is(err, hypervisor.ErrNotFound) {
			t.Fatalf("DeleteVolume(%q): %v", name, err)
		}
	}
	if names := h.volumeNames(instancesPool); len(names) != 0 {
		t.Fatalf("the fixture still holds %v", names)
	}

	if err := h.launch(t, running.ID); err != nil {
		t.Fatalf("LaunchHandler() on a terminating instance: %v", err)
	}
	if state, _ := h.state(t, running.ID); state != string(instance.StateTerminating) {
		t.Fatalf("state = %q, want terminating: the launch took it over", state)
	}
	if names := h.volumeNames(instancesPool); len(names) != 0 {
		t.Fatalf("the launch recreated %v", names)
	}
	if _, err := h.hv.GetDomain(t.Context(), running.ID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("GetDomain() = %v, want ErrNotFound: the launch booted a guest", err)
	}
}

func TestConsoleOutputServesTheTailOfTheLog(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	out, err := h.svc.ConsoleOutput(t.Context(), running.ID, 0)
	if err != nil {
		t.Fatalf("ConsoleOutput(): %v", err)
	}
	if out.InstanceID != running.ID {
		t.Errorf("instance_id = %q, want %q", out.InstanceID, running.ID)
	}
	if !strings.Contains(out.Output, "booting") {
		t.Errorf("the output does not carry the boot line:\n%s", out.Output)
	}

	// The tail is bounded, so a caller cannot ask for a log the size of the disk.
	h.appendConsole(t, running.ID, strings.Repeat("x", 4096))
	tailed, err := h.svc.ConsoleOutput(t.Context(), running.ID, 1)
	if err != nil {
		t.Fatalf("ConsoleOutput(1 KiB): %v", err)
	}
	if len(tailed.Output) > 1<<10 {
		t.Errorf("the output is %d bytes, want at most 1024", len(tailed.Output))
	}
}

func TestConsoleOutputRefusesAnUnacceptableTail(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	for _, tail := range []int{-1, instance.MaxConsoleTailKiB + 1} {
		_, err := h.svc.ConsoleOutput(t.Context(), running.ID, tail)
		var invalid *service.ValidationError
		if !errors.As(err, &invalid) {
			t.Fatalf("ConsoleOutput(%d) = %v, want a ValidationError", tail, err)
		}
		if !hasFieldReason(invalid, "tail_kib", fmt.Sprintf(
			"must be between %d and %d", instance.MinConsoleTailKiB, instance.MaxConsoleTailKiB)) {
			t.Errorf("ConsoleOutput(%d) details = %v", tail, invalid.Fields)
		}
	}
}

func TestConsoleOutputOfAnUnknownInstanceIsNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	if _, err := h.svc.ConsoleOutput(t.Context(), "i-0123456789abcdef0", 0); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("ConsoleOutput() = %v, want ErrNotFound", err)
	}
}

func TestConsoleOutputOfAnInstanceWithoutADomainIsNotFound(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	// The row outlives the domain, so this is a real answer: there is no log to
	// serve, and saying so beats serving an empty string as if it were the console.
	if err := h.hv.Undefine(t.Context(), running.ID); err != nil {
		t.Fatalf("Undefine(): %v", err)
	}
	if _, err := h.svc.ConsoleOutput(t.Context(), running.ID, 0); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("ConsoleOutput() = %v, want ErrNotFound", err)
	}
}

func TestUpdateRenamesAndRetags(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	name := "web-2"
	// The clock is frozen, so a rename in the same second as the launch would write
	// the same updated_at. Step forward to see that the row moved.
	h.advance()
	updated, err := h.svc.Update(t.Context(), running.ID, instance.UpdateInput{
		Name: &name,
		Tags: map[string]string{"env": "staging"},
	}, false)
	if err != nil {
		t.Fatalf("Update(): %v", err)
	}
	if updated.Name != name {
		t.Errorf("name = %q, want %q", updated.Name, name)
	}
	if updated.Tags["env"] != "staging" {
		t.Errorf("tags = %v, want env=staging", updated.Tags)
	}
	if updated.UpdatedAt.Equal(running.UpdatedAt) {
		t.Error("updated_at did not move; the row does not say the rename happened")
	}

	// An empty map is how every tag goes, which is not the same as saying nothing
	// about the tags.
	cleared, err := h.svc.Update(t.Context(), running.ID, instance.UpdateInput{
		Tags: map[string]string{},
	}, false)
	if err != nil {
		t.Fatalf("Update() clearing the tags: %v", err)
	}
	if len(cleared.Tags) != 0 {
		t.Errorf("tags = %v, want none", cleared.Tags)
	}
	if cleared.Name != name {
		t.Errorf("name = %q, want the rename to survive a tag update", cleared.Name)
	}
}

func TestUpdateRefusesWhatItCannotDo(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	running := h.runningInstance(t, "web")

	other := h.runningInstance(t, "db")
	bad := "Not A Label"
	empty := ""
	renamed := "web-2"

	// Each of these is refused before the service reads anything: the request
	// itself is not acceptable, so there is nothing to look up.
	for name, in := range map[string]instance.UpdateInput{
		"an invalid name":     {Name: &bad},
		"an empty name":       {Name: &empty},
		"an empty tag key":    {Tags: map[string]string{"": "value"}},
		"an overlong tag":     {Tags: map[string]string{"key": strings.Repeat("v", 257)}},
		"a body with nothing": {},
	} {
		_, err := h.svc.Update(t.Context(), running.ID, in, false)
		var invalid *service.ValidationError
		if !errors.As(err, &invalid) {
			t.Errorf("Update() with %s = %v, want a ValidationError", name, err)
		}
	}

	// A name that is taken is a conflict rather than a malformed request: the
	// partial unique index on the name refuses it, and the instance keeps the name
	// it had.
	if _, err := h.svc.Update(t.Context(), running.ID,
		instance.UpdateInput{Name: &other.Name}, false); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Update() to a taken name = %v, want ErrAlreadyExists", err)
	}
	if got, _ := h.svc.Get(t.Context(), running.ID, false); got.Name != running.Name {
		t.Errorf("the refused rename left the name %q, want %q", got.Name, running.Name)
	}

	// An instance that is not there is a 404, not a rename of nothing.
	if _, err := h.svc.Update(t.Context(), "i-0123456789abcdef0",
		instance.UpdateInput{Name: &renamed}, false); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Update() on an unknown instance = %v, want ErrNotFound", err)
	}
}

// runningImageOf names the image volume an instance's root disk is a delta of, so a
// terminate can be checked for leaving the images pool alone.
func (h *harness) runningImageOf(t *testing.T, instanceID string) string {
	t.Helper()

	rootDisk := hypervisor.RootDiskName(instanceID)
	for _, volume := range h.hv.Volumes() {
		if volume.Name == rootDisk {
			return volume.Backing
		}
	}
	t.Fatalf("the root disk %q is missing, so the fixture is not a launched instance", rootDisk)
	return ""
}

// appendConsole writes to the console of a domain, so a tail test has something to
// cut off.
func (h *harness) appendConsole(t *testing.T, instanceID, text string) {
	t.Helper()

	if err := h.hv.WriteConsole(t.Context(), instanceID, text); err != nil {
		t.Fatalf("WriteConsole(): %v", err)
	}
}
