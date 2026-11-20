package managedcluster_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

func names(nodes []managedcluster.Node) string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return strings.Join(out, " ")
}

func TestWorkerNaming(t *testing.T) {
	t.Parallel()
	if got := managedcluster.WorkerName("orders", "default", 3); got != "orders-default-3" {
		t.Errorf("WorkerName() = %q", got)
	}
	for name, want := range map[string]int{"orders-default-12": 12, "orders-cp-1": 1, "orders": 0, "a-b-x": 0} {
		if got := managedcluster.WorkerIndex(name); got != want {
			t.Errorf("WorkerIndex(%q) = %d, want %d", name, got, want)
		}
	}
	workers := []managedcluster.Node{{Name: "o-default-10"}, {Name: "o-default-2"}, {Name: "o-default-1"}}
	if got := managedcluster.HighestIndex(workers); got != 10 {
		t.Errorf("HighestIndex() = %d, want 10", got)
	}
	if got := names(managedcluster.SortedByIndex(workers)); got != "o-default-1 o-default-2 o-default-10" {
		t.Errorf("SortedByIndex() = %s (numeric, not lexical, order)", got)
	}
}

func TestSurplusIsNewestFirst(t *testing.T) {
	t.Parallel()
	workers := []managedcluster.Node{{Name: "o-d-1"}, {Name: "o-d-3"}, {Name: "o-d-2"}, {Name: "o-d-4"}}
	tests := map[int]string{4: "", 5: "", 3: "o-d-4", 1: "o-d-4 o-d-3 o-d-2", 0: "o-d-4 o-d-3 o-d-2 o-d-1", -1: "o-d-4 o-d-3 o-d-2 o-d-1"}
	for desired, want := range tests {
		if got := names(managedcluster.Surplus(workers, desired)); got != want {
			t.Errorf("Surplus(%d) = %q, want %q", desired, got, want)
		}
	}
}

var errBadPayload = errors.New("bad payload")

func TestDecodePayload(t *testing.T) {
	t.Parallel()
	raw, _ := json.Marshal(managedcluster.Payload{ClusterID: "eks-0123456789abcdef0"})
	if id, err := managedcluster.DecodePayload(worker.Task{ID: "t", Payload: raw}, errBadPayload); err != nil || id != "eks-0123456789abcdef0" {
		t.Errorf("DecodePayload() = %q, %v", id, err)
	}
	for _, bad := range []string{`{}`, `not json`, `{"cluster_id":""}`} {
		if _, err := managedcluster.DecodePayload(worker.Task{ID: "t", Payload: json.RawMessage(bad)}, errBadPayload); !errors.Is(err, errBadPayload) {
			t.Errorf("DecodePayload(%s) = %v, want the module's error", bad, err)
		}
	}
}

var errCanceled = errors.New("canceledCtx by a delete")

func TestWait(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := managedcluster.Wait{Timeout: 50 * time.Millisecond, Interval: time.Millisecond, Canceled: errCanceled}

	calls := 0
	if err := w.For(ctx, "c", "it", func(context.Context) bool { calls++; return calls == 3 }); err != nil || calls != 3 {
		t.Errorf("For() = %v after %d calls, want nil after 3", err, calls)
	}
	if err := w.For(ctx, "c", "it", func(context.Context) bool { return false }); !errors.Is(err, managedcluster.ErrTimedOut) {
		t.Errorf("For() of a condition that never holds = %v, want ErrTimedOut", err)
	}
	w.Deleting = func(context.Context) bool { return true }
	if err := w.For(ctx, "c", "it", func(context.Context) bool { return false }); !errors.Is(err, errCanceled) {
		t.Errorf("For() while deleting = %v, want the module's cancellation", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	w.Deleting = nil
	w.Timeout = time.Hour
	if err := w.For(canceledCtx, "c", "it", func(context.Context) bool { return false }); !errors.Is(err, context.Canceled) {
		t.Errorf("For() with a canceledCtx context = %v", err)
	}
}

func TestEndpointRecordIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	hv := fake.New(fake.Config{})
	if err := hv.DefineNetwork(ctx, hypervisor.NetworkSpec{
		Name: "net", Bridge: "br0", CIDR: "10.0.0.0/24", Gateway: "10.0.0.1", Mode: hypervisor.NetworkModeNAT,
	}); err != nil {
		t.Fatal(err)
	}
	if err := hv.StartNetwork(ctx, "net"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := managedcluster.PublishEndpoint(ctx, hv, "net", "10.0.0.10", "api.orders.default"); err != nil {
			t.Fatalf("PublishEndpoint(): %v", err)
		}
	}
	for range 2 {
		if err := managedcluster.RemoveEndpoint(ctx, hv, "net", "10.0.0.10", "api.orders.default"); err != nil {
			t.Fatalf("RemoveEndpoint(): %v", err)
		}
	}
	if err := managedcluster.RemoveEndpoint(ctx, hv, "net", "", "api.orders.default"); err != nil {
		t.Errorf("RemoveEndpoint() with no address = %v, want nothing to do", err)
	}
	if err := managedcluster.PublishEndpoint(ctx, hv, "missing", "10.0.0.10", "x"); err == nil {
		t.Error("PublishEndpoint() on a missing network succeeded")
	}
}

var errTear = errors.New("libvirt said no")

func TestTeardown(t *testing.T) {
	t.Parallel()
	var torn []string
	tear := func(_ context.Context, id string) error {
		if id == "i-bad" {
			return errTear
		}
		torn = append(torn, id)
		return nil
	}
	nodes := []managedcluster.Node{{InstanceID: "i-1", Name: "a"}, {Name: "never-reserved"}, {InstanceID: "i-2", Name: "b"}}
	if err := managedcluster.Teardown(t.Context(), tear, nodes); err != nil || strings.Join(torn, " ") != "i-1 i-2" {
		t.Errorf("Teardown() = %v, tore %v", err, torn)
	}
	err := managedcluster.Teardown(t.Context(), tear, []managedcluster.Node{{InstanceID: "i-bad", Name: "c"}})
	if !errors.Is(err, errTear) || !strings.Contains(err.Error(), "c") {
		t.Errorf("Teardown() of a failing node = %v, want the error naming it", err)
	}
}

func TestJudge(t *testing.T) {
	t.Parallel()
	const threshold = 3
	tests := []struct {
		name               string
		degraded           bool
		reason, lastReason string
		failures           int
		want               managedcluster.Verdict
	}{
		{"healthy and active", false, "", "", 0, managedcluster.Keep},
		{"one bad pass", false, "down", "", 1, managedcluster.Keep},
		{"the third bad pass", false, "down", "", 3, managedcluster.Degrade},
		{"healthy again", true, "", "down", 0, managedcluster.Recover},
		{"still down, same reason", true, "down", "down", 5, managedcluster.Keep},
		{"still down, new reason", true, "worse", "down", 6, managedcluster.Refresh},
	}
	for _, tc := range tests {
		if got := managedcluster.Judge(tc.degraded, tc.reason, tc.lastReason, tc.failures, threshold); got != tc.want {
			t.Errorf("%s: Judge() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHealthMemory(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	h := managedcluster.NewHealth(30*time.Second, func() time.Time { return now })

	if !h.Due("c") || h.Due("c") {
		t.Error("a cluster is due once per interval")
	}
	now = now.Add(31 * time.Second)
	if !h.Due("c") {
		t.Error("a cluster is due again after the interval")
	}
	if h.Record("c", false) != 1 || h.Record("c", false) != 2 || h.Record("c", true) != 0 {
		t.Error("Record() counts unhealthy passes in a row and resets on a healthy one")
	}
	h.RecordNodes("c", map[string]bool{"n1": true})
	seen := h.Nodes("c")
	seen["n1"] = false // a copy: the caller cannot change the memory
	if !h.Nodes("c")["n1"] {
		t.Error("Nodes() handed out the memory itself")
	}
	h.Forget("c")
	if len(h.Nodes("c")) != 0 || h.Record("c", false) != 1 || !h.Due("c") {
		t.Error("Forget() left something behind")
	}
}
