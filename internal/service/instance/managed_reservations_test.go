package instance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// errModuleWrite stands for a module's own row failing to be written.
var errModuleWrite = errors.New("the module's row could not be written")

// clusterReservations is a Kubernetes cluster's VMs as the module will ask for
// them: a control plane and n workers.
func clusterReservations(imageID string, workers int) []instance.ManagedReservation {
	ins := []instance.ManagedReservation{{
		Name: "orders-cp-1", Owner: instance.ManagedByKubernetes, ImageID: imageID,
		InstanceType: instanceType, RootDiskGB: imageDiskGB,
	}}
	for i := 1; i <= workers; i++ {
		ins = append(ins, instance.ManagedReservation{
			Name: fmt.Sprintf("orders-default-%d", i), Owner: instance.ManagedByKubernetes, ImageID: imageID,
			InstanceType: instanceType, RootDiskGB: imageDiskGB,
		})
	}
	return ins
}

func countInstances(t *testing.T, h harness) int {
	t.Helper()
	var n int
	if err := h.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM instances`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A cluster's VMs are reserved together, each with its own address, in the
// transaction the module writes its own rows in.
func TestManagedReservationsReserveAClusterTogether(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "kubernetes-1.37")

	var got []instance.ManagedReserved
	err := h.svc.WithManagedReservations(t.Context(), clusterReservations(source.ID, 2),
		func(ctx context.Context, tx *store.Tx, reserved []instance.ManagedReserved) error {
			// The rows are visible inside the transaction the module extends.
			for _, r := range reserved {
				if _, err := tx.GetInstance(ctx, sqlc.GetInstanceParams{ID: r.InstanceID}); err != nil {
					return fmt.Errorf("the row of %s is not in the transaction: %w", r.Name, err)
				}
			}
			got = reserved
			return nil
		})
	if err != nil {
		t.Fatalf("WithManagedReservations(): %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("reserved %d VMs, want 3", len(got))
	}
	ips := map[string]bool{}
	for i, want := range []string{"orders-cp-1", "orders-default-1", "orders-default-2"} {
		if got[i].Name != want {
			t.Errorf("reserved[%d] = %q, want %q: the order of the request", i, got[i].Name, want)
		}
		if ips[got[i].PrivateIP] {
			t.Errorf("%s was given %s twice: each address must see the ones before it", got[i].Name, got[i].PrivateIP)
		}
		ips[got[i].PrivateIP] = true
		row, err := h.store.GetInstance(t.Context(), sqlc.GetInstanceParams{ID: got[i].InstanceID})
		if err != nil {
			t.Fatalf("GetInstance(): %v", err)
		}
		if instance.ManagedBy(row.ManagedBy) != instance.ManagedByKubernetes {
			t.Errorf("%s managed_by = %q, want eks", row.Name, row.ManagedBy)
		}
		if row.KeyPairID.Valid || row.UserData.Valid {
			t.Errorf("%s carries a key pair or user data; a node is no machine anyone has a shell on", row.Name)
		}
	}
	if got[0].SubnetID != got[2].SubnetID || got[0].VPCID != got[2].VPCID {
		t.Error("the VMs of one reservation landed in different subnets")
	}
}

// All or nothing: eight small VMs each fit on the node alone, and do not fit
// together. The sum is refused before anything is written, rather than the
// cluster being reserved up to the VM that does not fit.
func TestManagedReservationsAreAllOrNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "kubernetes-1.37")
	before := countInstances(t, h)

	called := false
	err := h.svc.WithManagedReservations(t.Context(), clusterReservations(source.ID, 7),
		func(context.Context, *store.Tx, []instance.ManagedReserved) error {
			called = true
			return nil
		})
	if !errors.Is(err, service.ErrQuota) {
		t.Fatalf("error = %v, want ErrQuota for a cluster the node cannot hold", err)
	}
	if called {
		t.Error("the module's function ran for a reservation that was refused")
	}
	if after := countInstances(t, h); after != before {
		t.Errorf("instances = %d after the refusal, want %d: nothing may be written", after, before)
	}

	// The same VMs one at a time would have fitted: it is the sum that is
	// checked, which is the point.
	if err := h.svc.WithManagedReservation(t.Context(), clusterReservations(source.ID, 0)[0],
		func(context.Context, *store.Tx, instance.ManagedReserved) error { return nil }); err != nil {
		t.Errorf("one VM of the same shape: %v, want it to fit", err)
	}
}

// A failure of the module's own writes takes every VM of the reservation with
// it, because they are one transaction.
func TestManagedReservationsRollBackTogether(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "kubernetes-1.37")
	before := countInstances(t, h)

	err := h.svc.WithManagedReservations(t.Context(), clusterReservations(source.ID, 2),
		func(context.Context, *store.Tx, []instance.ManagedReserved) error { return errModuleWrite })
	if !errors.Is(err, errModuleWrite) {
		t.Fatalf("error = %v, want the module's error", err)
	}
	if after := countInstances(t, h); after != before {
		t.Errorf("instances = %d, want %d: a failed module write must leave no VM reserved", after, before)
	}
}

func TestManagedReservationsRefuseAMixedRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	source := h.availableImage(t, "kubernetes-1.37")

	tests := map[string]struct {
		mutate func([]instance.ManagedReservation)
		want   error
	}{
		"two owners": {func(ins []instance.ManagedReservation) {
			ins[1].Owner = instance.ManagedByDatabases
		}, service.ErrInvalid},
		"two subnets": {func(ins []instance.ManagedReservation) {
			ins[1].SubnetID = "subnet-0123456789abcdef0"
		}, service.ErrInvalid},
		"no owner": {func(ins []instance.ManagedReservation) {
			for i := range ins {
				ins[i].Owner = instance.ManagedByNone
			}
		}, service.ErrInvalid},
		"a name twice": {func(ins []instance.ManagedReservation) {
			ins[2].Name = ins[1].Name
		}, nil},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ins := clusterReservations(source.ID, 2)
			tc.mutate(ins)
			err := h.svc.WithManagedReservations(t.Context(), ins,
				func(context.Context, *store.Tx, []instance.ManagedReserved) error { return nil })
			if err == nil {
				t.Fatal("the reservation was accepted")
			}
			var invalid *service.ValidationError
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if tc.want == nil && !errors.As(err, &invalid) {
				t.Errorf("error = %v, want a validation error", err)
			}
		})
	}

	if err := h.svc.WithManagedReservations(t.Context(), nil,
		func(context.Context, *store.Tx, []instance.ManagedReserved) error { return nil }); !errors.Is(err, service.ErrInvalid) {
		t.Errorf("an empty reservation: error = %v, want ErrInvalid", err)
	}
}

// adoptAsK8sNode turns an instance the harness launched into a node of a
// Kubernetes cluster, writing the rows by hand because the service that owns
// them is M8.5's.
func adoptAsK8sNode(t *testing.T, h harness, instanceID, clusterID, clusterName string) {
	t.Helper()
	ctx := t.Context()

	if _, err := h.store.DB().ExecContext(ctx,
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByKubernetes), instanceID); err != nil {
		t.Fatalf("mark %s managed: %v", instanceID, err)
	}
	row, err := h.store.GetInstance(ctx, sqlc.GetInstanceParams{ID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	var vpcID, subnetID string
	if err := h.store.DB().QueryRowContext(ctx,
		`SELECT s.vpc_id, s.id FROM network_interfaces n JOIN subnets s ON s.id = n.subnet_id
		 WHERE n.instance_id = ?`, instanceID).Scan(&vpcID, &subnetID); err != nil {
		t.Fatalf("read the node's subnet: %v", err)
	}
	if _, err := h.store.CreateK8sCluster(ctx, sqlc.CreateK8sClusterParams{
		ID: clusterID, Name: clusterName, KubernetesVersion: "1.37", ImageID: row.ImageID,
		VpcID: vpcID, SubnetID: subnetID, PodCidr: "100.64.0.0/16", ServiceCidr: "100.96.0.0/16",
		ControlPlaneCount: 1, ControlPlaneType: row.InstanceType, State: "active",
		CaCertPem: "-----BEGIN CERTIFICATE-----\n", CreatedAt: row.LaunchedAt,
	}); err != nil {
		t.Fatalf("CreateK8sCluster(): %v", err)
	}
	if err := h.store.CreateK8sNode(ctx, sqlc.CreateK8sNodeParams{
		InstanceID: instanceID, ClusterID: clusterID, Role: "control-plane",
	}); err != nil {
		t.Fatalf("CreateK8sNode(): %v", err)
	}
}

// Every instance write on a node answers 409 and says which cluster to act
// through, as a DB instance's VM does.
func TestLifecycleRefusesAKubernetesNode(t *testing.T) {
	t.Parallel()

	const (
		clusterID = "eks-0a1b2c3d4e5f60718"
		name      = "orders"
	)
	calls := map[string]func(context.Context, *instance.Service, string) error{
		"stop": func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Stop(ctx, id, false)
			return err
		},
		"forced stop": func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Stop(ctx, id, true)
			return err
		},
		"start": func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Start(ctx, id)
			return err
		},
		"reboot": func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Reboot(ctx, id, false)
			return err
		},
		"terminate": func(ctx context.Context, svc *instance.Service, id string) error {
			_, err := svc.Terminate(ctx, id)
			return err
		},
		"rename": func(ctx context.Context, svc *instance.Service, id string) error {
			n := "renamed"
			_, err := svc.Update(ctx, id, instance.UpdateInput{Name: &n}, false)
			return err
		},
	}
	for action, call := range calls {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			node := launchRunning(t, h, "orders-cp-1")
			adoptAsK8sNode(t, h, node, clusterID, name)

			err := call(t.Context(), h.svc, node)
			if !errors.Is(err, service.ErrConflict) {
				t.Fatalf("error = %v, want a conflict", err)
			}
			for _, want := range []string{clusterID, name, "/api/v1/eks/clusters"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

// A node is hidden from the Instances pages, like any managed VM.
func TestListHidesKubernetesNodes(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	node := launchRunning(t, h, "orders-cp-1")
	adoptAsK8sNode(t, h, node, "eks-0a1b2c3d4e5f60718", "orders")
	mine := launchRunning(t, h, "web-1")

	page, err := h.svc.List(t.Context(), instance.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != mine {
		t.Errorf("List() = %+v, want the user's instance alone", page.Items)
	}
}

// A node whose cluster row cannot be read is still refused, and still points
// at the right API.
func TestLifecycleRefusesAKubernetesNodeWithNoClusterRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	node := launchRunning(t, h, "orders-cp-1")
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedByKubernetes), node); err != nil {
		t.Fatal(err)
	}
	_, err := h.svc.Terminate(t.Context(), node)
	if !errors.Is(err, service.ErrConflict) || !strings.Contains(err.Error(), "/api/v1/eks/clusters") {
		t.Errorf("error = %v, want a conflict pointing at the Kubernetes API", err)
	}
}

// The schema holds the shape section 3 describes: a control plane belongs to
// no node group, and a worker always belongs to one.
func TestK8sNodeRolesAndGroupsAgree(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	cp := launchRunning(t, h, "orders-cp-1")
	adoptAsK8sNode(t, h, cp, "eks-0a1b2c3d4e5f60718", "orders")
	worker := launchRunning(t, h, "orders-default-1")
	if _, err := h.store.CreateK8sNodeGroup(t.Context(), sqlc.CreateK8sNodeGroupParams{
		ID: "ng-0a1b2c3d4e5f60718", ClusterID: "eks-0a1b2c3d4e5f60718", Name: "default",
		InstanceType: instanceType, RootDiskGb: 20, DesiredSize: 1, CreatedAt: "2026-11-20T12:00:00Z",
	}); err != nil {
		t.Fatalf("CreateK8sNodeGroup(): %v", err)
	}
	group := sql.NullString{String: "ng-0a1b2c3d4e5f60718", Valid: true}

	if err := h.store.CreateK8sNode(t.Context(), sqlc.CreateK8sNodeParams{
		InstanceID: worker, ClusterID: "eks-0a1b2c3d4e5f60718", Role: "worker",
	}); err == nil {
		t.Error("a worker without a node group was accepted")
	}
	if err := h.store.CreateK8sNode(t.Context(), sqlc.CreateK8sNodeParams{
		InstanceID: worker, ClusterID: "eks-0a1b2c3d4e5f60718", NodeGroupID: group, Role: "control-plane",
	}); err == nil {
		t.Error("a control plane in a node group was accepted")
	}
	if err := h.store.CreateK8sNode(t.Context(), sqlc.CreateK8sNodeParams{
		InstanceID: worker, ClusterID: "eks-0a1b2c3d4e5f60718", NodeGroupID: group, Role: "worker",
	}); err != nil {
		t.Errorf("a worker in its node group: %v", err)
	}

	nodes, err := h.store.ListK8sNodes(t.Context(), sqlc.ListK8sNodesParams{ClusterID: "eks-0a1b2c3d4e5f60718"})
	if err != nil {
		t.Fatalf("ListK8sNodes(): %v", err)
	}
	if len(nodes) != 2 || nodes[0].Role != "control-plane" || nodes[1].Role != "worker" {
		t.Errorf("ListK8sNodes() = %+v, want the control plane then the worker", nodes)
	}
}
