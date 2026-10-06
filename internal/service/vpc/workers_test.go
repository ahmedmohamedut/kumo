// Worker tests for the VPC workflows. They drive the handlers directly with a
// worker.Task, the same way the image tests do: the pool itself is covered by
// the worker package, what matters here is that the create and delete handlers
// bring the fake hypervisor to the state the row describes.
package vpc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// errBridgeDown is what the fake hypervisor is told to return when the start
// half of define-then-start fails. Package-level (the bootstrap tests use the
// same sentinel), so the wrap assertions stay exact.
var errBridgeDown = errors.New("the bridge is down")

// createTask is the worker task a real vpc.create runs.
func createTask(vpcID string) worker.Task {
	return worker.Task{
		ID: "task-test", Type: task.TypeVPCCreate,
		Payload: []byte(fmt.Sprintf(`{"vpc_id":%q}`, vpcID)),
	}
}

// deleteTask is the worker task a real vpc.delete runs.
func deleteTask(vpcID string) worker.Task {
	return worker.Task{
		ID: "task-test", Type: task.TypeVPCDelete,
		Payload: []byte(fmt.Sprintf(`{"vpc_id":%q}`, vpcID)),
	}
}

// createVPC is a convenience for the tests that need one row on disk.
func createVPC(t *testing.T, h harness) VPC {
	t.Helper()

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.10.0.0/16",
		Tags: map[string]string{testEnvTag: testVPCName},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	return created.VPC
}

func TestHandlerRegistersBothTaskTypes(t *testing.T) {
	h := newHarness(t)

	registry := h.svc.Handler()
	if len(registry) != 2 {
		t.Fatalf("registry = %v, want exactly the two VPC task types", registry)
	}
	if _, ok := registry[task.TypeVPCCreate]; !ok {
		t.Errorf("no handler for %q", task.TypeVPCCreate)
	}
	if _, ok := registry[task.TypeVPCDelete]; !ok {
		t.Errorf("no handler for %q", task.TypeVPCDelete)
	}
}

func TestCreateHandlerDefinesAndStartsTheNetwork(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	ctx := context.Background()
	if err := h.svc.CreateHandler(ctx, createTask(created.ID)); err != nil {
		t.Fatalf("CreateHandler() error = %v", err)
	}

	networks := h.hv.Networks()
	if len(networks) != 1 {
		t.Fatalf("defined %d networks, want exactly one", len(networks))
	}
	net := networks[0]
	if net.Name != created.ID {
		t.Errorf("network name = %q, want the VPC id %q", net.Name, created.ID)
	}
	if net.Bridge != created.BridgeName {
		t.Errorf("bridge = %q, want %q", net.Bridge, created.BridgeName)
	}
	if net.CIDR != created.CIDR || net.Gateway != created.GatewayIP {
		t.Errorf("cidr/gateway = %q/%q, want %q/%q", net.CIDR, net.Gateway, created.CIDR, created.GatewayIP)
	}
	if net.Mode != hypervisor.NetworkModeNAT {
		t.Errorf("mode = %q, want nat", net.Mode)
	}
	if net.Domain != created.Name {
		t.Errorf("dns domain = %q, want the VPC name %q", net.Domain, created.Name)
	}
	if !net.Active {
		t.Error("network is not active after the create task")
	}
}

func TestCreateHandlerIsIdempotent(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	ctx := context.Background()
	if err := h.svc.CreateHandler(ctx, createTask(created.ID)); err != nil {
		t.Fatalf("first CreateHandler() error = %v", err)
	}

	// A requeued task (a crash between the define and the finish) adopts the
	// network instead of defining a second one.
	if err := h.svc.CreateHandler(ctx, createTask(created.ID)); err != nil {
		t.Fatalf("second CreateHandler() error = %v", err)
	}
	if networks := h.hv.Networks(); len(networks) != 1 || !networks[0].Active {
		t.Errorf("networks = %+v, want the one network still active", networks)
	}
}

func TestCreateHandlerAdoptsADefinedInactiveNetwork(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	// An interrupted earlier attempt left the definition behind; the retry must
	// start it, never redefine it.
	row, err := h.store.GetVPC(context.Background(), sqlc.GetVPCParams{ID: created.ID})
	if err != nil {
		t.Fatalf("GetVPC() error = %v", err)
	}
	if err := h.hv.DefineNetwork(context.Background(), networkSpec(row)); err != nil {
		t.Fatalf("DefineNetwork() error = %v", err)
	}

	if err := h.svc.CreateHandler(context.Background(), createTask(created.ID)); err != nil {
		t.Fatalf("CreateHandler() error = %v", err)
	}

	networks := h.hv.Networks()
	if len(networks) != 1 {
		t.Fatalf("defined %d networks, want exactly one", len(networks))
	}
	if !networks[0].Active {
		t.Error("the adopted network was not started")
	}
	if networks[0].Bridge != row.BridgeName {
		t.Errorf("the retry rewrote the bridge to %q, want %q", networks[0].Bridge, row.BridgeName)
	}
}

func TestCreateHandlerSkipsAVanishedRow(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	if _, err := h.store.Exec(context.Background(),
		"DELETE FROM vpcs WHERE id = ?", created.ID); err != nil {
		t.Fatalf("delete the VPC row: %v", err)
	}

	if err := h.svc.CreateHandler(context.Background(), createTask(created.ID)); err != nil {
		t.Fatalf("CreateHandler() error = %v, want a skip once the row is gone", err)
	}
	if len(h.hv.Networks()) != 0 {
		t.Errorf("the handler defined %d networks for a deleted VPC", len(h.hv.Networks()))
	}
}

func TestCreateHandlerRollsBackWhenStartFails(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)
	h.hv.FailAlways(hypervisor.OpStartNetwork, errBridgeDown)

	err := h.svc.CreateHandler(context.Background(), createTask(created.ID))
	if err == nil {
		t.Fatalf("CreateHandler() error = nil, want a failure")
	}

	// The definition was removed again, so the retry starts from a clean slate.
	if networks := h.hv.Networks(); len(networks) != 0 {
		t.Errorf("the failed create left %d networks behind: %+v", len(networks), networks)
	}
}

func TestCreateHandlerRejectsBadPayloads(t *testing.T) {
	h := newHarness(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "a payload without a vpc id", body: `{}`},
		{name: "an empty vpc id", body: `{"vpc_id":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := h.svc.CreateHandler(context.Background(), worker.Task{
				ID: "task-test", Type: task.TypeVPCCreate, Payload: []byte(tt.body),
			})
			if !errors.Is(err, ErrBadTaskPayload) {
				t.Errorf("CreateHandler() error = %v, want ErrBadTaskPayload", err)
			}
		})
	}

	// Malformed JSON is a decode failure, which must read as a bug rather than
	// as a task for nothing.
	t.Run("a malformed payload", func(t *testing.T) {
		err := h.svc.CreateHandler(context.Background(), worker.Task{
			ID: "task-test", Type: task.TypeVPCCreate, Payload: []byte(`{`),
		})
		if err == nil || errors.Is(err, ErrBadTaskPayload) {
			t.Errorf("CreateHandler() error = %v, want a payload decode failure", err)
		}
	})
}

func TestDeleteHandlerRemovesTheNetworkTheRowAndTheTags(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	ctx := context.Background()
	if err := h.svc.CreateHandler(ctx, createTask(created.ID)); err != nil {
		t.Fatalf("CreateHandler() error = %v", err)
	}

	if err := h.svc.DeleteHandler(ctx, deleteTask(created.ID)); err != nil {
		t.Fatalf("DeleteHandler() error = %v", err)
	}

	// The network, the row and the tags all go together.
	if len(h.hv.Networks()) != 0 {
		t.Errorf("the delete left %d networks behind", len(h.hv.Networks()))
	}
	if _, err := h.store.GetVPC(ctx, sqlc.GetVPCParams{ID: created.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetVPC() after the delete = %v, want no rows", err)
	}
	var tags int
	if err := h.store.DB().QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tags WHERE resource_id = ?", created.ID).Scan(&tags); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if tags != 0 {
		t.Errorf("%d orphan tags remain after the delete", tags)
	}
}

func TestDeleteHandlerIsIdempotent(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	ctx := context.Background()
	if err := h.svc.DeleteHandler(ctx, deleteTask(created.ID)); err != nil {
		t.Fatalf("first DeleteHandler() error = %v", err)
	}

	// A requeued delete whose first run already removed everything is a skip.
	if err := h.svc.DeleteHandler(ctx, deleteTask(created.ID)); err != nil {
		t.Fatalf("second DeleteHandler() error = %v, want a skip once the row is gone", err)
	}
}

func TestDeleteHandlerRechecksSubnets(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	ctx := context.Background()
	if err := h.svc.CreateHandler(ctx, createTask(created.ID)); err != nil {
		t.Fatalf("CreateHandler() error = %v", err)
	}
	if _, err := h.sub.Create(ctx, subnet.CreateInput{
		VPCID: created.ID, Name: "app-a", CIDR: "10.10.0.0/24",
	}); err != nil {
		t.Fatalf("subnet.Create() error = %v", err)
	}

	// A subnet created between the delete request and its task keeps the network
	// alive even though the API already refused to queue the delete.
	err := h.svc.DeleteHandler(ctx, deleteTask(created.ID))
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("DeleteHandler() error = %v, want ErrConflict while a subnet exists", err)
	}
	if networks := h.hv.Networks(); len(networks) != 1 || !networks[0].Active {
		t.Errorf("the refused delete touched the network: %+v", networks)
	}
	if _, err := h.store.GetVPC(ctx, sqlc.GetVPCParams{ID: created.ID}); err != nil {
		t.Errorf("GetVPC() error = %v, want the row to survive the refused delete", err)
	}
}

func TestDeleteHandlerToleratesANeverCreatedNetwork(t *testing.T) {
	h := newHarness(t)
	created := createVPC(t, h)

	// The create task never ran, or ran against a hypervisor that lost the
	// definition; deleting the row is still the desired end state.
	if err := h.svc.DeleteHandler(context.Background(), deleteTask(created.ID)); err != nil {
		t.Fatalf("DeleteHandler() error = %v", err)
	}
	if _, err := h.store.GetVPC(context.Background(), sqlc.GetVPCParams{ID: created.ID}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetVPC() after the delete = %v, want no rows", err)
	}
}
