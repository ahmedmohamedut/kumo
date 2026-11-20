package kubernetes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/k8scloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// DefaultDrainTimeout bounds the drain of one node in a scale-down (section 4).
const DefaultDrainTimeout = 5 * time.Minute

// Scale sets a node group's size and queues k8s.scale (section 4). A scale up
// reserves the new VMs now, in one transaction with the new size, their node
// rows and the task, so a size the node cannot hold is refused with nothing
// changed. A scale down only records the size: which nodes go, and their
// drain, is the task's. A size equal to the current one changes nothing and
// queues nothing.
func (s *Service) Scale(ctx context.Context, clusterID, groupID string, desired int) (Cluster, error) {
	if desired < MinNodeCount || desired > MaxNodeCount {
		return Cluster{}, invalid(service.FieldError{Field: "desired_size",
			Reason: fmt.Sprintf("must be between %d and %d", MinNodeCount, MaxNodeCount)})
	}
	current, err := s.Get(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
	}
	if current.State != StateActive && current.State != StateDegraded {
		return Cluster{}, fmt.Errorf("%w: cluster %q is %s; a node group is resized once it is active or degraded",
			service.ErrConflict, clusterID, current.State)
	}
	group, err := s.store.GetK8sNodeGroup(ctx, sqlc.GetK8sNodeGroupParams{ID: groupID, ClusterID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, fmt.Errorf("%w: cluster %q has no node group %q", service.ErrNotFound, clusterID, groupID)
		}
		return Cluster{}, fmt.Errorf("read node group %q: %w", groupID, err)
	}
	if int(group.DesiredSize) == desired {
		return current, nil
	}

	row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
	if err != nil {
		return Cluster{}, fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	l, err := s.layout(ctx, row)
	if err != nil {
		return Cluster{}, err
	}

	var adds []instance.ManagedReservation
	if desired > len(l.workers) {
		next := managedcluster.HighestIndex(l.workers) + 1
		for i := 0; i < desired-len(l.workers); i++ {
			adds = append(adds, instance.ManagedReservation{
				Name: managedcluster.WorkerName(row.Name, group.Name, next+i), Owner: instance.ManagedByKubernetes,
				ImageID: row.ImageID, InstanceType: group.InstanceType, SubnetID: row.SubnetID,
				RootDiskGB: int(group.RootDiskGb),
			})
		}
	}

	var taskID string
	write := func(ctx context.Context, tx *store.Tx, reserved []instance.ManagedReserved) error {
		now := formatTime(s.now())
		if _, err := tx.SetK8sNodeGroupSize(ctx, sqlc.SetK8sNodeGroupSizeParams{
			ID: group.ID, DesiredSize: int64(desired), UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("resize node group %q: %w", group.ID, err)
		}
		for _, vm := range reserved {
			if err := tx.CreateK8sNode(ctx, sqlc.CreateK8sNodeParams{
				InstanceID: vm.InstanceID, ClusterID: clusterID, Role: RoleWorker,
				NodeGroupID: sql.NullString{String: group.ID, Valid: true},
			}); err != nil {
				return fmt.Errorf("record node %s: %w", vm.Name, err)
			}
		}
		if _, err := tx.SetK8sClusterState(ctx, sqlc.SetK8sClusterStateParams{
			ID: clusterID, State: string(StateUpdating), UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("mark cluster %q updating: %w", clusterID, err)
		}
		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeK8sClusterScale, clusterID,
			managedcluster.Payload{ClusterID: clusterID})
		if err != nil {
			return err
		}
		taskID = queued.ID
		return nil
	}
	if len(adds) > 0 {
		err = s.instances.WithManagedReservations(ctx, adds, write)
	} else {
		err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error { return write(ctx, tx, nil) })
	}
	if err != nil {
		return Cluster{}, err
	}

	out, err := s.Get(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
	}
	out.TaskID = &taskID
	s.logger.InfoContext(ctx, "kubernetes: scale queued", "cluster_id", clusterID,
		"node_group", group.Name, "from", group.DesiredSize, "to", desired, "task_id", taskID)
	return out, nil
}

// ScaleHandler runs k8s.scale: workers beyond the desired size are drained and
// removed, newest first; reserved workers not yet built are launched and
// joined. The cluster ends active, or degraded with the reason -- a drain a
// disruption budget held past the limit, or a new node that never became
// Ready. Requeued, it adopts what an earlier run built and drains again.
func (s *Service) ScaleHandler(ctx context.Context, t worker.Task) error {
	clusterID, err := decodePayload(t)
	if err != nil {
		return err
	}
	release, err := s.locks.Acquire(ctx, LockKey(clusterID), t.Type)
	if err != nil {
		return fmt.Errorf("take the lock on cluster %q: %w", clusterID, err)
	}
	defer release()

	row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	if State(row.State) != StateUpdating {
		return nil // a delete overtook it, or a requeue of a finished scale
	}
	reason, err := s.scale(ctx, row)
	if err != nil {
		// A shutdown is requeued, and a delete that arrived owns the row now:
		// writing degraded over deleting would report a failure for a cluster
		// that is simply going away.
		if ctx.Err() != nil || errors.Is(err, ErrCreateCancelled) {
			return err
		}
		s.setState(context.WithoutCancel(ctx), row.ID, StateDegraded, "the scale failed: "+err.Error())
		return err
	}
	if reason != "" {
		s.setState(ctx, row.ID, StateDegraded, reason)
		return nil
	}
	s.setState(ctx, row.ID, StateActive, "")
	return nil
}

// scale does the work and returns why the cluster is degraded, if it is.
func (s *Service) scale(ctx context.Context, row sqlc.K8sCluster) (string, error) {
	l, err := s.layout(ctx, row)
	if err != nil {
		return "", err
	}
	groups, err := s.store.ListK8sNodeGroups(ctx, sqlc.ListK8sNodeGroupsParams{ClusterID: row.ID})
	if err != nil || len(groups) == 0 {
		return "", fmt.Errorf("read the node group of cluster %q: %w", row.ID, err)
	}
	desired := int(groups[0].DesiredSize)
	pki, err := k8spki.LoadDir(s.pkiDir(row.ID))
	if err != nil {
		return "", fmt.Errorf("read the PKI of cluster %q: %w", row.ID, err)
	}
	api, err := s.api(s.connection(l, pki))
	if err != nil {
		return "", fmt.Errorf("open the API of cluster %q: %w", row.ID, err)
	}
	defer api.Close()
	if err := api.Ready(ctx); err != nil {
		return "", fmt.Errorf("the API server: %w", err)
	}

	workers := managedcluster.SortedByIndex(l.workers)

	// Down: the newest first, each drained before its VM goes.
	if surplus := managedcluster.Surplus(workers, desired); len(surplus) > 0 {
		for _, w := range surplus {
			if blocked, err := s.drain(ctx, api, w); err != nil {
				return "", err
			} else if blocked != "" {
				return blocked, nil
			}
			if err := s.removeWorker(ctx, api, w); err != nil {
				return "", err
			}
		}
		return "", nil
	}

	// Up: the workers reserved by Scale that have no running VM yet.
	var pending []managedcluster.Node
	states := map[string]string{}
	nodes, err := s.store.ListK8sNodes(ctx, sqlc.ListK8sNodesParams{ClusterID: row.ID})
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		states[n.InstanceID] = n.State
	}
	for _, w := range workers {
		if states[w.InstanceID] == string(instance.StatePending) {
			pending = append(pending, w)
		}
	}
	if len(pending) == 0 {
		return "", nil
	}
	token, err := k8spki.NewBootstrapToken(nil)
	if err != nil {
		return "", err
	}
	if err := api.CreateBootstrapToken(ctx, token, s.now().Add(workerTokenTTL)); err != nil {
		return "", fmt.Errorf("mint the new workers' bootstrap token: %w", err)
	}
	hash, err := k8spki.CAHash(pki.CA.CertPEM)
	if err != nil {
		return "", err
	}
	launched := map[string]instance.ManagedLaunched{}
	for _, w := range pending {
		got, err := s.instances.LaunchManaged(ctx, w.InstanceID, instance.ManagedLaunch{
			Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
				userData, err := k8scloudinit.WorkerUserData(k8scloudinit.WorkerRequest{
					NodeName: w.Name, Endpoint: l.endpoint, ControlPlaneIP: l.controlPlane.PrivateIP,
					NodeIP: w.PrivateIP, Token: token, CAHash: hash,
				})
				if err != nil {
					return hypervisor.CloudInit{}, err
				}
				return managedcluster.Seed(w, userData), nil
			},
		})
		if err != nil {
			return "", fmt.Errorf("launch worker %s: %w", w.Name, err)
		}
		launched[w.InstanceID] = got
	}
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for id, got := range launched {
			if err := s.instances.MarkManagedRunningInTx(ctx, tx, id, got); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return "", err
	}

	var notReady []string
	err = s.await(ctx, row.ID, "the new nodes to be Ready", func(ctx context.Context) bool {
		notReady = s.unreadyNodes(ctx, row.ID, api, l.nodeNames())
		return len(notReady) == 0
	})
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, managedcluster.ErrTimedOut):
		return "nodes not Ready after the scale: " + strings.Join(notReady, ", "), nil
	default:
		return "", err
	}
}

// drain cordons a worker and evicts its pods (section 4): every pod but its
// DaemonSet and mirror pods, which go with the node. An eviction a disruption
// budget refuses is retried until the drain limit; past it the drain stops and
// names the pods, and the node is kept -- a scale never deletes past a PDB.
func (s *Service) drain(ctx context.Context, api ClusterAPI, w managedcluster.Node) (string, error) {
	if err := api.Cordon(ctx, w.Name); err != nil && !errors.Is(err, k8sapi.ErrNotFound) {
		return "", fmt.Errorf("cordon %s: %w", w.Name, err)
	}
	deadline := time.Now().Add(s.drainTimeout)
	for {
		pods, err := api.PodsOnNode(ctx, w.Name)
		if err != nil {
			return "", fmt.Errorf("list the pods on %s: %w", w.Name, err)
		}
		var blocked []string
		for _, p := range pods {
			if p.DaemonSet || p.Mirror {
				continue
			}
			if err := api.Evict(ctx, p.Namespace, p.Name); err != nil {
				if errors.Is(err, k8sapi.ErrEvictionBlocked) {
					blocked = append(blocked, p.Namespace+"/"+p.Name)
					continue
				}
				return "", fmt.Errorf("evict %s/%s from %s: %w", p.Namespace, p.Name, w.Name, err)
			}
		}
		if len(blocked) == 0 {
			return "", nil
		}
		if time.Now().After(deadline) {
			sort.Strings(blocked)
			return fmt.Sprintf("the drain of %s stopped after %s: a disruption budget would not let %s move",
				w.Name, s.drainTimeout, strings.Join(blocked, ", ")), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.pollInterval):
		}
	}
}

// removeWorker deletes the Node, then the VM, then the rows.
func (s *Service) removeWorker(ctx context.Context, api ClusterAPI, w managedcluster.Node) error {
	if err := api.DeleteNode(ctx, w.Name); err != nil {
		return fmt.Errorf("delete node %s: %w", w.Name, err)
	}
	if err := s.tear(ctx, w.InstanceID); err != nil {
		return fmt.Errorf("tear down %s: %w", w.Name, err)
	}
	return s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := tx.DeleteK8sNode(ctx, sqlc.DeleteK8sNodeParams{InstanceID: w.InstanceID}); err != nil {
			return err
		}
		return s.instances.DeleteManagedInTx(ctx, tx, w.InstanceID)
	})
}
