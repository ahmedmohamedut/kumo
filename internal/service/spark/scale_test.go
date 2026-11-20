package spark_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/spark"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

func workerNames(c spark.Cluster) string {
	var out []string
	for _, n := range c.Nodes {
		if n.Role == spark.RoleWorker {
			out = append(out, n.Name)
		}
	}
	return strings.Join(out, " ")
}

func (h *harness) scale(t *testing.T, c spark.Cluster, desired int) spark.Cluster {
	t.Helper()
	queued, err := h.svc.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, desired)
	if err != nil {
		t.Fatalf("Scale(%d): %v", desired, err)
	}
	if queued.State != spark.StateUpdating || queued.TaskID == nil {
		t.Fatalf("Scale(%d) = %s with task %v, want updating with a task", desired, queued.State, queued.TaskID)
	}
	if err := h.run(t, c.ID, h.svc.ScaleHandler); err != nil {
		t.Fatalf("ScaleHandler(): %v", err)
	}
	return h.get(t, c.ID)
}

func TestScaleUpRegistersNewWorkers(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")

	got := h.scale(t, c, 4)
	if got.State != spark.StateActive || got.NodeGroups[0].DesiredSize != 4 {
		t.Fatalf("after the scale: %s, desired %d", got.State, got.NodeGroups[0].DesiredSize)
	}
	if w := workerNames(got); w != "etl-default-1 etl-default-2 etl-default-3 etl-default-4" {
		t.Errorf("workers = %s", w)
	}
	for _, n := range got.Nodes {
		if n.State != "running" {
			t.Errorf("%s is %s, want running", n.Name, n.State)
		}
	}
	// The new workers carry the cluster's secret, read back from its
	// directory, and join the same master.
	spk, _, _ := readSecrets(t, h, c.ID)
	seed := h.seedOf(t, got.Nodes[4].InstanceID)
	if !strings.Contains(seed, spk) || !strings.Contains(seed, "SPARK_MASTER_URL=spark://"+got.Nodes[0].PrivateIP+":7077") {
		t.Error("a new worker's seed does not join the cluster")
	}
}

// Scale down removes the newest workers first, each through a graceful
// shutdown, which is what drains it (S13).
func TestScaleDownDecommissionsTheNewestFirst(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.scale(t, h.active(t, "etl"), 3)
	removed := map[string]string{}
	for _, n := range c.Nodes {
		removed[n.Name] = n.InstanceID
	}

	got := h.scale(t, c, 1)
	if got.State != spark.StateActive || workerNames(got) != "etl-default-1" {
		t.Fatalf("after the scale: %s (%v), workers %s", got.State, got.StateReason, workerNames(got))
	}
	for _, name := range []string{"etl-default-2", "etl-default-3"} {
		if _, err := h.hv.GetDomain(t.Context(), removed[name]); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("%s's domain is still there", name)
		}
		if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: removed[name]}); err == nil {
			t.Errorf("%s's row is still there", name)
		}
	}
	// Each got an ACPI shutdown, not a hard stop: the fake records the
	// request on the console.
	if !strings.Contains(h.logs.String(), "decommissioning a worker") {
		t.Error("no worker was decommissioned")
	}
}

// A worker whose VM does not power off in time is forced off and still
// removed, and the cluster says so: its work was lost.
func TestScaleDownForcesAWorkerThatWillNotStop(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{objects: true, s3Endpoint: s3Endpoint, ignoreShutdown: true})
	h.sparkImage(t, "4.3")
	h.bucket(t, "data")
	h.bucket(t, "logs")
	c := h.active(t, "etl")

	got := h.scale(t, c, 1)
	if got.State != spark.StateDegraded || got.StateReason == nil ||
		!strings.Contains(*got.StateReason, "etl-default-2") || !strings.Contains(*got.StateReason, "powered off") {
		t.Fatalf("after the scale: %s (%v), want degraded naming the forced worker", got.State, got.StateReason)
	}
	if workerNames(got) != "etl-default-1" {
		t.Errorf("workers = %s, want the forced worker removed too", workerNames(got))
	}
}

func TestScaleUpDegradesWhenANewWorkerNeverRegisters(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")
	queued, err := h.svc.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	h.api.set(true, queued.Nodes[3].PrivateIP)
	if err := h.run(t, c.ID, h.svc.ScaleHandler); err != nil {
		t.Fatal(err)
	}
	got := h.get(t, c.ID)
	if got.State != spark.StateDegraded || !strings.Contains(*got.StateReason, "etl-default-3") {
		t.Errorf("state = %s (%v), want degraded naming etl-default-3", got.State, got.StateReason)
	}
}

func TestScaleRefusals(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")
	group := c.NodeGroups[0].ID

	for _, size := range []int{0, 11} {
		var invalid *service.ValidationError
		if _, err := h.svc.Scale(t.Context(), c.ID, group, size); !errors.As(err, &invalid) {
			t.Errorf("Scale(%d) = %v, want a validation error", size, err)
		}
	}
	if _, err := h.svc.Scale(t.Context(), c.ID, "ng-0123456789abcdef0", 3); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Scale() of another group = %v, want ErrNotFound", err)
	}
	same, err := h.svc.Scale(t.Context(), c.ID, group, 2)
	if err != nil || same.TaskID != nil || same.State != spark.StateActive {
		t.Errorf("Scale() to the same size = %+v, %v, want unchanged", same, err)
	}
	if _, err := h.svc.Scale(t.Context(), c.ID, group, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Scale(t.Context(), c.ID, group, 1); !errors.Is(err, service.ErrConflict) {
		t.Errorf("a second Scale() while updating = %v, want a conflict", err)
	}
}

// A delete that arrives while a scale waits owns the row: the scale stands
// down rather than writing over deleting.
func TestScaleStandsDownForADelete(t *testing.T) {
	t.Parallel()
	h := ready(t)
	c := h.active(t, "etl")
	if _, err := h.svc.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Delete(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	err := h.svc.ScaleHandler(t.Context(), worker.Task{ID: "task-scale", Type: task.TypeSparkClusterScale,
		ResourceID: c.ID, Payload: json.RawMessage(`{"cluster_id":"` + c.ID + `"}`)})
	if err != nil {
		t.Errorf("ScaleHandler() = %v, want it to stand down quietly", err)
	}
	row, err := h.store.GetSparkCluster(t.Context(), sqlc.GetSparkClusterParams{ID: c.ID})
	if err != nil || row.State != string(spark.StateDeleting) {
		t.Errorf("state = %s (%v), want deleting kept", row.State, err)
	}
}
