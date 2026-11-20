package spark

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// drainMargin is how long past the drain timeout a worker's VM gets to power
// off before it is forced: the OS's own shutdown after spark-worker stopped.
const drainMargin = time.Minute

// Scale sets a node group's size and queues spark.scale (section 4). A scale
// up reserves the new VMs now, in one transaction with the new size, their
// node rows and the task, so a size the node cannot hold is refused with
// nothing changed. A scale down only records the size: which workers go, and
// their decommission, is the task's. The same size changes nothing.
func (s *Service) Scale(ctx context.Context, clusterID, groupID string, desired int) (Cluster, error) {
	if desired < MinWorkerCount || desired > MaxWorkerCount {
		return Cluster{}, invalid(service.FieldError{Field: "desired_size",
			Reason: fmt.Sprintf("must be between %d and %d", MinWorkerCount, MaxWorkerCount)})
	}
	current, err := s.Get(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
	}
	if current.State != StateActive && current.State != StateDegraded {
		return Cluster{}, fmt.Errorf("%w: cluster %q is %s; a node group is resized once it is active or degraded",
			service.ErrConflict, clusterID, current.State)
	}
	group, err := s.store.GetSparkNodeGroup(ctx, sqlc.GetSparkNodeGroupParams{ID: groupID, ClusterID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Cluster{}, fmt.Errorf("%w: cluster %q has no node group %q", service.ErrNotFound, clusterID, groupID)
		}
		return Cluster{}, fmt.Errorf("read node group %q: %w", groupID, err)
	}
	if int(group.DesiredSize) == desired {
		return current, nil
	}
	row, err := s.row(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
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
				Name: managedcluster.WorkerName(row.Name, group.Name, next+i), Owner: instance.ManagedBySpark,
				ImageID: row.ImageID, InstanceType: group.InstanceType, SubnetID: row.SubnetID,
				RootDiskGB: int(group.RootDiskGb),
			})
		}
	}

	var taskID string
	write := func(ctx context.Context, tx *store.Tx, reserved []instance.ManagedReserved) error {
		now := formatTime(s.now())
		if _, err := tx.SetSparkNodeGroupSize(ctx, sqlc.SetSparkNodeGroupSizeParams{
			ID: group.ID, DesiredSize: int64(desired), UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("resize node group %q: %w", group.ID, err)
		}
		for _, vm := range reserved {
			if err := tx.CreateSparkNode(ctx, sqlc.CreateSparkNodeParams{
				InstanceID: vm.InstanceID, ClusterID: clusterID, Role: RoleWorker,
				NodeGroupID: sql.NullString{String: group.ID, Valid: true},
			}); err != nil {
				return fmt.Errorf("record node %s: %w", vm.Name, err)
			}
		}
		if _, err := tx.SetSparkClusterState(ctx, sqlc.SetSparkClusterStateParams{
			ID: clusterID, State: string(StateUpdating), UpdatedAt: now,
		}); err != nil {
			return fmt.Errorf("mark cluster %q updating: %w", clusterID, err)
		}
		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeSparkClusterScale, clusterID,
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
	s.logger.InfoContext(ctx, "spark: scale queued", "cluster_id", clusterID,
		"node_group", group.Name, "from", group.DesiredSize, "to", desired, "task_id", taskID)
	return out, nil
}

// ScaleHandler runs spark.scale: workers beyond the desired size are
// decommissioned and removed, newest first; reserved workers not yet built are
// launched and registered. The cluster ends active, or degraded with the
// reason -- a worker that would not finish its decommission in time, or a new
// one that never registered. Requeued, it adopts what an earlier run built.
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

	row, err := s.store.GetSparkCluster(ctx, sqlc.GetSparkClusterParams{ID: clusterID})
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
		// A shutdown is requeued, and a delete that arrived owns the row.
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
func (s *Service) scale(ctx context.Context, row sqlc.SparkCluster) (string, error) {
	l, err := s.layout(ctx, row)
	if err != nil {
		return "", err
	}
	groups, err := s.store.ListSparkNodeGroups(ctx, sqlc.ListSparkNodeGroupsParams{ClusterID: row.ID})
	if err != nil || len(groups) == 0 {
		return "", fmt.Errorf("read the node group of cluster %q: %w", row.ID, err)
	}
	desired := int(groups[0].DesiredSize)

	// Down: the newest first, each decommissioned through its own shutdown
	// (S13) before its VM goes.
	if surplus := managedcluster.Surplus(l.workers, desired); len(surplus) > 0 {
		var forced []string
		for _, w := range surplus {
			drained, err := s.decommission(ctx, w)
			if err != nil {
				return "", err
			}
			if !drained {
				forced = append(forced, w.Name)
			}
			if err := s.removeWorker(ctx, w); err != nil {
				return "", err
			}
		}
		if len(forced) > 0 {
			return fmt.Sprintf("%s did not finish decommissioning within %s and was powered off: "+
				"work on it was lost and re-run elsewhere", strings.Join(forced, ", "), s.drainTimeout), nil
		}
		return "", nil
	}

	// Up: the workers reserved by Scale that have no running VM yet.
	nodes, err := s.store.ListSparkNodes(ctx, sqlc.ListSparkNodesParams{ClusterID: row.ID})
	if err != nil {
		return "", err
	}
	states := map[string]string{}
	for _, n := range nodes {
		states[n.InstanceID] = n.State
	}
	secret, _, err := s.secrets(row.ID)
	if err != nil {
		return "", err
	}
	launched := map[string]instance.ManagedLaunched{}
	for _, w := range l.workers {
		if states[w.InstanceID] != string(instance.StatePending) {
			continue
		}
		req, err := s.workerRequest(ctx, l, w, secret)
		if err != nil {
			return "", err
		}
		got, err := s.instances.LaunchManaged(ctx, w.InstanceID, instance.ManagedLaunch{
			Seed: func(context.Context) (hypervisor.CloudInit, error) {
				userData, err := sparkcloudinit.WorkerUserData(req)
				if err != nil {
					return hypervisor.CloudInit{}, err
				}
				return managedcluster.Seed(w, userData.Value()), nil
			},
		})
		if err != nil {
			return "", fmt.Errorf("launch worker %s: %w", w.Name, err)
		}
		launched[w.InstanceID] = got
	}
	if len(launched) == 0 {
		return "", nil
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

	api, err := s.api(s.connection(l))
	if err != nil {
		return "", fmt.Errorf("open the status of cluster %q: %w", row.ID, err)
	}
	defer api.Close()
	var missing []string
	err = s.await(ctx, row.ID, "the new workers to register", func(ctx context.Context) bool {
		missing = s.unregistered(ctx, l, api)
		return len(missing) == 0
	})
	switch {
	case err == nil:
		return "", nil
	case errors.Is(err, managedcluster.ErrTimedOut):
		return "workers not registered after the scale: " + strings.Join(missing, ", "), nil
	default:
		return "", err
	}
}

// decommission shuts a worker's VM down gracefully: its spark-worker unit
// drains it on the way (S13, packaging/images/spark-4.3), bounded by the
// drain timeout the worker's seed carries. It reports whether the VM powered
// off in time; one that did not is forced off by the teardown that follows.
func (s *Service) decommission(ctx context.Context, w managedcluster.Node) (bool, error) {
	if err := s.hv.Shutdown(ctx, w.InstanceID); err != nil {
		if errors.Is(err, hypervisor.ErrNotFound) || errors.Is(err, hypervisor.ErrInvalidState) {
			return true, nil // already gone or already off
		}
		return false, fmt.Errorf("shut down %s: %w", w.Name, err)
	}
	s.logger.InfoContext(ctx, "spark: decommissioning a worker", "node", w.Name, "instance_id", w.InstanceID)
	deadline := time.Now().Add(s.decommWait)
	for {
		d, err := s.hv.GetDomain(ctx, w.InstanceID)
		if errors.Is(err, hypervisor.ErrNotFound) || (err == nil && d.State == hypervisor.DomainStateShutoff) {
			return true, nil
		}
		if time.Now().After(deadline) {
			s.logger.WarnContext(ctx, "spark: a worker did not power off in time; forcing it",
				"node", w.Name, "drain_timeout", s.drainTimeout)
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(s.pollInterval):
		}
	}
}

// removeWorker tears the VM down and deletes its rows.
func (s *Service) removeWorker(ctx context.Context, w managedcluster.Node) error {
	if err := s.tear(ctx, w.InstanceID); err != nil {
		return fmt.Errorf("tear down %s: %w", w.Name, err)
	}
	return s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := tx.DeleteSparkNode(ctx, sqlc.DeleteSparkNodeParams{InstanceID: w.InstanceID}); err != nil {
			return err
		}
		return s.instances.DeleteManagedInTx(ctx, tx, w.InstanceID)
	})
}
