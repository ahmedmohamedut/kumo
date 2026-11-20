package instance_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

const sparkClusterID = "spk-0a1b2c3d4e5f60718"

// adoptAsSparkMaster makes an instance the master of a Spark cluster, the way
// the M9.5 create will, with nothing else of the cluster's present.
func adoptAsSparkMaster(t *testing.T, h harness, instanceID, clusterName string) {
	t.Helper()
	ctx := t.Context()

	if _, err := h.store.DB().ExecContext(ctx,
		`UPDATE instances SET managed_by = ? WHERE id = ?`,
		string(instance.ManagedBySpark), instanceID); err != nil {
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
	if _, err := h.store.CreateSparkCluster(ctx, sqlc.CreateSparkClusterParams{
		ID: sparkClusterID, Name: clusterName, SparkVersion: "4.3", ImageID: row.ImageID,
		VpcID: vpcID, SubnetID: subnetID, MasterType: row.InstanceType, State: "active",
		CreatedAt: row.LaunchedAt,
	}); err != nil {
		t.Fatalf("CreateSparkCluster(): %v", err)
	}
	if err := h.store.CreateSparkNode(ctx, sqlc.CreateSparkNodeParams{
		InstanceID: instanceID, ClusterID: sparkClusterID, Role: "master",
	}); err != nil {
		t.Fatalf("CreateSparkNode(): %v", err)
	}
}

// A Spark node is refused every instance write, with the cluster to act
// through named, as a Kubernetes node is.
func TestLifecycleRefusesASparkNode(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	node := launchRunning(t, h, "etl-master")
	adoptAsSparkMaster(t, h, node, "etl")

	for action, call := range map[string]func() error{
		"stop":      func() error { _, err := h.svc.Stop(t.Context(), node, false); return err },
		"reboot":    func() error { _, err := h.svc.Reboot(t.Context(), node, false); return err },
		"terminate": func() error { _, err := h.svc.Terminate(t.Context(), node); return err },
	} {
		err := call()
		if !errors.Is(err, service.ErrConflict) ||
			!strings.Contains(err.Error(), "/api/v1/spark/clusters/"+sparkClusterID) ||
			!strings.Contains(err.Error(), `"etl"`) {
			t.Errorf("%s: error = %v, want a conflict naming the cluster and its API", action, err)
		}
	}

	// And it is on no Instances page.
	page, err := h.svc.List(t.Context(), instance.ListInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.ID == node {
			t.Error("a Spark node is listed among the instances")
		}
	}
}

func TestLifecycleRefusesASparkNodeWithNoClusterRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	node := launchRunning(t, h, "etl-master")
	if _, err := h.store.DB().ExecContext(t.Context(),
		`UPDATE instances SET managed_by = ? WHERE id = ?`, string(instance.ManagedBySpark), node); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.Terminate(t.Context(), node); !errors.Is(err, service.ErrConflict) ||
		!strings.Contains(err.Error(), "/api/v1/spark/clusters") {
		t.Errorf("error = %v, want a conflict pointing at the Spark API", err)
	}
}

// The schema holds what section 3 describes: the master belongs to no node
// group and a worker always to one; an access key exactly with object storage;
// an event-log bucket only with object storage.
func TestSparkSchemaConstraints(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	master := launchRunning(t, h, "etl-master")
	adoptAsSparkMaster(t, h, master, "etl")
	worker := launchRunning(t, h, "etl-default-1")
	ctx := t.Context()

	if _, err := h.store.CreateSparkNodeGroup(ctx, sqlc.CreateSparkNodeGroupParams{
		ID: "ng-0a1b2c3d4e5f60718", ClusterID: sparkClusterID, Name: "default",
		InstanceType: instanceType, RootDiskGb: 8, DesiredSize: 1, CreatedAt: "2026-11-21T12:00:00Z",
	}); err != nil {
		t.Fatalf("CreateSparkNodeGroup(): %v", err)
	}
	group := sql.NullString{String: "ng-0a1b2c3d4e5f60718", Valid: true}
	if err := h.store.CreateSparkNode(ctx, sqlc.CreateSparkNodeParams{
		InstanceID: worker, ClusterID: sparkClusterID, Role: "worker",
	}); err == nil {
		t.Error("a worker without a node group was accepted")
	}
	if err := h.store.CreateSparkNode(ctx, sqlc.CreateSparkNodeParams{
		InstanceID: worker, ClusterID: sparkClusterID, NodeGroupID: group, Role: "master",
	}); err == nil {
		t.Error("a master in a node group was accepted")
	}
	if err := h.store.CreateSparkNode(ctx, sqlc.CreateSparkNodeParams{
		InstanceID: worker, ClusterID: sparkClusterID, NodeGroupID: group, Role: "worker",
	}); err != nil {
		t.Errorf("a worker in its node group: %v", err)
	}
	nodes, err := h.store.ListSparkNodes(ctx, sqlc.ListSparkNodesParams{ClusterID: sparkClusterID})
	if err != nil || len(nodes) != 2 || nodes[0].Role != "master" || nodes[1].Role != "worker" {
		t.Errorf("ListSparkNodes() = %+v, %v, want the master then the worker", nodes, err)
	}

	// The cluster-level checks, against a row that is otherwise valid.
	cluster, err := h.store.GetSparkCluster(ctx, sqlc.GetSparkClusterParams{ID: sparkClusterID})
	if err != nil {
		t.Fatal(err)
	}
	valid := sqlc.CreateSparkClusterParams{
		Name: "x", SparkVersion: "4.3", ImageID: cluster.ImageID, VpcID: cluster.VpcID,
		SubnetID: cluster.SubnetID, MasterType: cluster.MasterType, State: "creating",
		CreatedAt: "2026-11-21T12:00:00Z",
	}
	cases := []struct {
		name string
		edit func(*sqlc.CreateSparkClusterParams)
		ok   bool
	}{
		{"object storage with its key", func(p *sqlc.CreateSparkClusterParams) {
			p.ObjectStorage, p.AccessKeyID = 1, sql.NullString{String: "KUMOABC", Valid: true}
		}, true},
		{"object storage with no key", func(p *sqlc.CreateSparkClusterParams) { p.ObjectStorage = 1 }, false},
		{"a key with object storage off", func(p *sqlc.CreateSparkClusterParams) {
			p.AccessKeyID = sql.NullString{String: "KUMOABC", Valid: true}
		}, false},
		{"an event-log bucket with object storage off", func(p *sqlc.CreateSparkClusterParams) {
			p.EventLogBucket = sql.NullString{String: "logs", Valid: true}
		}, false},
		{"an event-log bucket with object storage", func(p *sqlc.CreateSparkClusterParams) {
			p.ObjectStorage, p.AccessKeyID = 1, sql.NullString{String: "KUMOABC", Valid: true}
			p.EventLogBucket = sql.NullString{String: "logs", Valid: true}
		}, true},
	}
	for i, tc := range cases {
		p := valid
		p.ID = "spk-0a1b2c3d4e5f6071" + string(rune('a'+i))
		p.Name = "c" + string(rune('a'+i))
		tc.edit(&p)
		_, err := h.store.CreateSparkCluster(ctx, p)
		if tc.ok != (err == nil) {
			t.Errorf("%s: err = %v, want accepted = %v", tc.name, err, tc.ok)
		}
	}

	// The buckets go with the cluster.
	if err := h.store.AddSparkClusterBucket(ctx, sqlc.AddSparkClusterBucketParams{
		ClusterID: sparkClusterID, Bucket: "data",
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := h.store.ListSparkClusterBuckets(ctx, sqlc.ListSparkClusterBucketsParams{ClusterID: sparkClusterID}); err != nil ||
		len(got) != 1 || got[0] != "data" {
		t.Errorf("ListSparkClusterBuckets() = %v, %v", got, err)
	}
}
