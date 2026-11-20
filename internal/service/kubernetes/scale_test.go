package kubernetes_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/kubernetes"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

func workerNames(c kubernetes.Cluster) []string {
	var out []string
	for _, n := range c.Nodes {
		if n.Role == kubernetes.RoleWorker {
			out = append(out, n.Name)
		}
	}
	return out
}

func (h *harness) scale(t *testing.T, c kubernetes.Cluster, desired int) kubernetes.Cluster {
	t.Helper()
	queued, err := h.svc.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, desired)
	if err != nil {
		t.Fatalf("Scale(%d): %v", desired, err)
	}
	if queued.State != kubernetes.StateUpdating || queued.TaskID == nil {
		t.Fatalf("Scale(%d) = %s with task %v, want updating with a task", desired, queued.State, queued.TaskID)
	}
	if err := h.run(t, c.ID, h.svc.ScaleHandler); err != nil {
		t.Fatalf("ScaleHandler(): %v", err)
	}
	return h.get(t, c.ID)
}

func TestScaleUpJoinsNewWorkers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")

	got := h.scale(t, c, 4)
	if got.State != kubernetes.StateActive || got.NodeGroups[0].DesiredSize != 4 {
		t.Fatalf("after the scale: %s, desired %d", got.State, got.NodeGroups[0].DesiredSize)
	}
	if w := strings.Join(workerNames(got), " "); w != "orders-default-1 orders-default-2 orders-default-3 orders-default-4" {
		t.Errorf("workers = %s", w)
	}
	for _, n := range got.Nodes {
		if n.State != "running" {
			t.Errorf("%s is %s, want running", n.Name, n.State)
		}
	}
	// The create's token and one for the scale, each minted by the API.
	if len(h.api.tokens) != 2 {
		t.Errorf("%d tokens minted, want 2", len(h.api.tokens))
	}
}

// Scale down drains the newest workers first: cordon, evict what is not a
// DaemonSet's or a mirror pod, delete the node, then the VM.
func TestScaleDownDrainsTheNewestFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")
	c = h.scale(t, c, 3)
	h.api.pods["orders-default-3"] = []k8sapi.Pod{
		{Namespace: "default", Name: "web-1"},
		{Namespace: "kube-system", Name: "cilium-x", DaemonSet: true},
		{Namespace: "kube-system", Name: "static", Mirror: true},
	}
	h.api.pods["orders-default-2"] = []k8sapi.Pod{{Namespace: "default", Name: "web-2"}}
	removed := map[string]string{}
	for _, n := range c.Nodes {
		removed[n.Name] = n.InstanceID
	}
	h.api.calls = nil

	got := h.scale(t, c, 1)
	if got.State != kubernetes.StateActive || strings.Join(workerNames(got), " ") != "orders-default-1" {
		t.Fatalf("after the scale: %s, workers %v", got.State, workerNames(got))
	}
	want := "cordon orders-default-3, evict default/web-1, delete orders-default-3, " +
		"cordon orders-default-2, evict default/web-2, delete orders-default-2"
	if got := strings.Join(h.api.calls, ", "); got != want {
		t.Errorf("calls = %s\nwant    %s", got, want)
	}
	for _, name := range []string{"orders-default-2", "orders-default-3"} {
		if _, err := h.hv.GetDomain(t.Context(), removed[name]); !errors.Is(err, hypervisor.ErrNotFound) {
			t.Errorf("%s's domain is still there", name)
		}
		if _, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: removed[name]}); err == nil {
			t.Errorf("%s's row is still there", name)
		}
	}
}

// A drain a disruption budget holds past the limit stops the scale: the node
// is kept, cordoned, and the cluster is degraded naming the pod.
func TestScaleDownNeverDeletesPastADisruptionBudget(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")
	h.api.pods["orders-default-2"] = []k8sapi.Pod{{Namespace: "default", Name: "guarded"}}
	h.api.blocked["default/guarded"] = true
	h.api.calls = nil

	got := h.scale(t, c, 1)
	if got.State != kubernetes.StateDegraded || got.StateReason == nil ||
		!strings.Contains(*got.StateReason, "default/guarded") || !strings.Contains(*got.StateReason, "orders-default-2") {
		t.Fatalf("after the scale: %s (%v), want degraded naming the pod and the node", got.State, got.StateReason)
	}
	if strings.Join(workerNames(got), " ") != "orders-default-1 orders-default-2" {
		t.Errorf("workers = %v, want the guarded node kept", workerNames(got))
	}
	for _, call := range h.api.calls {
		if strings.HasPrefix(call, "delete ") {
			t.Errorf("%s ran past a disruption budget", call)
		}
	}
	if !strings.Contains(strings.Join(h.api.calls, ","), "cordon orders-default-2") {
		t.Error("the node was not cordoned")
	}
}

func TestScaleRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")
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
	// The same size changes nothing and queues nothing.
	same, err := h.svc.Scale(t.Context(), c.ID, group, 2)
	if err != nil || same.TaskID != nil || same.State != kubernetes.StateActive {
		t.Errorf("Scale() to the same size = %+v, %v, want unchanged", same, err)
	}
	// A size the node cannot hold is refused with nothing changed.
	if _, err := h.svc.Scale(t.Context(), c.ID, group, 10); !errors.Is(err, service.ErrQuota) {
		t.Errorf("Scale(10) = %v, want ErrQuota on a 32 GiB node", err)
	}
	if got := h.get(t, c.ID); got.NodeGroups[0].DesiredSize != 2 || len(got.Nodes) != 3 || got.State != kubernetes.StateActive {
		t.Errorf("after the refused scale: desired %d, %d nodes, %s", got.NodeGroups[0].DesiredSize, len(got.Nodes), got.State)
	}
	// Only from active or degraded: one scale at a time.
	if _, err := h.svc.Scale(t.Context(), c.ID, group, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Scale(t.Context(), c.ID, group, 1); !errors.Is(err, service.ErrConflict) {
		t.Errorf("a second Scale() while updating = %v, want a conflict", err)
	}
}

// A delete that arrives while a scale-up waits for its nodes owns the row: the
// scale stands down rather than writing degraded over deleting.
func TestScaleStandsDownForADelete(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	c := h.active(t, "orders")
	if _, err := h.svc.Scale(t.Context(), c.ID, c.NodeGroups[0].ID, 3); err != nil {
		t.Fatal(err)
	}
	h.api.set(true, "orders-default-3") // the new node never becomes Ready
	if _, err := h.svc.Delete(t.Context(), c.ID); err != nil {
		t.Fatal(err)
	}
	// The scale task was queued first; run it now that the cluster is deleting.
	err := h.svc.ScaleHandler(t.Context(), worker.Task{ID: "task-scale", Type: task.TypeK8sClusterScale,
		ResourceID: c.ID, Payload: json.RawMessage(`{"cluster_id":"` + c.ID + `"}`)})
	if err != nil {
		t.Errorf("ScaleHandler() = %v, want it to stand down quietly", err)
	}
	row, err := h.store.GetK8sCluster(t.Context(), sqlc.GetK8sClusterParams{ID: c.ID})
	if err != nil || row.State != string(kubernetes.StateDeleting) {
		t.Errorf("state = %s (%v), want deleting kept", row.State, err)
	}
	if len(h.api.tokens) != 1 {
		t.Errorf("%d tokens minted, want the create's alone: the scale must not have built anything", len(h.api.tokens))
	}
}
