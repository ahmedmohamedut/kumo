package spark

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/sparkapi"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

func decodePayload(t worker.Task) (string, error) {
	return managedcluster.DecodePayload(t, ErrBadTaskPayload)
}

// layout is what every task reads before it acts.
type layout struct {
	row      sqlc.SparkCluster
	vpc      sqlc.Vpc
	master   managedcluster.Node
	workers  []managedcluster.Node
	types    map[string]string // node name -> instance type
	endpoint string
}

func (s *Service) layout(ctx context.Context, row sqlc.SparkCluster) (layout, error) {
	vpc, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: row.VpcID})
	if err != nil {
		return layout{}, fmt.Errorf("read the VPC of cluster %q: %w", row.ID, err)
	}
	nodes, err := s.store.ListSparkNodes(ctx, sqlc.ListSparkNodesParams{ClusterID: row.ID})
	if err != nil {
		return layout{}, fmt.Errorf("list the nodes of cluster %q: %w", row.ID, err)
	}
	out := layout{row: row, vpc: vpc, endpoint: endpointName(row.Name, vpc.Name), types: map[string]string{}}
	for _, n := range nodes {
		node := managedcluster.Node{InstanceID: n.InstanceID, Name: n.Name, Role: n.Role, PrivateIP: n.PrivateIp.String}
		out.types[n.Name] = n.InstanceType
		if n.Role == RoleMaster {
			out.master = node
		} else {
			out.workers = append(out.workers, node)
		}
	}
	return out, nil
}

func (l layout) workerIPs() []string {
	ips := make([]string, 0, len(l.workers))
	for _, w := range l.workers {
		ips = append(ips, w.PrivateIP)
	}
	return ips
}

func (s *Service) connection(l layout) Connection {
	return Connection{ClusterID: l.row.ID, MasterIP: l.master.PrivateIP, WorkerIPs: l.workerIPs()}
}

// s3 is the cluster's S3 settings, nil for a cluster without object storage.
func (s *Service) s3(row sqlc.SparkCluster, secret sparkcloudinit.Secret) *sparkcloudinit.S3 {
	if row.ObjectStorage == 0 || !row.AccessKeyID.Valid {
		return nil
	}
	return &sparkcloudinit.S3{Endpoint: s.s3Endpoint, Region: s.s3Region, AccessKey: row.AccessKeyID.String, SecretKey: secret}
}

// CreateHandler runs spark.create (section 4): the event-log marker, the
// master, its endpoint record, the workers, until every worker is registered.
//
// A master that never answers leaves the cluster failed with its VMs kept for
// their consoles, as a Kubernetes control plane does; workers that never
// register make it degraded, because the master works.
func (s *Service) CreateHandler(ctx context.Context, t worker.Task) error {
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
			return nil // deleted before the task ran
		}
		return fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	if State(row.State) != StateCreating {
		s.logger.InfoContext(ctx, "spark: create skipped", "cluster_id", row.ID, "state", row.State)
		return nil
	}
	if err := s.create(ctx, row); err != nil {
		if ctx.Err() != nil || errors.Is(err, ErrCreateCancelled) {
			return err
		}
		s.setState(context.WithoutCancel(ctx), row.ID, StateFailed, err.Error())
		return err
	}
	return nil
}

func (s *Service) create(ctx context.Context, row sqlc.SparkCluster) error {
	l, err := s.layout(ctx, row)
	if err != nil {
		return err
	}
	secret, s3Secret, err := s.secrets(row.ID)
	if err != nil {
		return err
	}
	s3 := s.s3(row, s3Secret)

	// 0. The event-log prefix: s3a reports a prefix with nothing under it as
	//    missing, and Spark refuses to log into a missing directory. The
	//    marker is idempotent, so a requeued create writes it again.
	if row.EventLogBucket.Valid {
		if s.objects == nil {
			return fmt.Errorf("%w: cluster %q has an event-log bucket and object storage is off", service.ErrConflict, row.ID)
		}
		key := "spark-events/" + row.ID + "/.keep"
		if err := s.objects.PutObject(ctx, row.EventLogBucket.String, key, bytes.NewReader(nil), 0, "application/octet-stream"); err != nil {
			return fmt.Errorf("write the event-log marker %s/%s: %w", row.EventLogBucket.String, key, err)
		}
	}

	// 1. The master. A requeued task adopts the seed it rendered before.
	launched := map[string]instance.ManagedLaunched{}
	master, err := s.instances.LaunchManaged(ctx, l.master.InstanceID, instance.ManagedLaunch{
		Seed: func(context.Context) (hypervisor.CloudInit, error) {
			req := sparkcloudinit.MasterRequest{ClusterID: row.ID, NodeName: l.master.Name,
				NodeIP: l.master.PrivateIP, Secret: secret}
			if row.EventLogBucket.Valid && s3 != nil {
				req.EventLog = &sparkcloudinit.EventLog{S3: *s3, Bucket: row.EventLogBucket.String}
			}
			userData, err := sparkcloudinit.MasterUserData(req)
			if err != nil {
				return hypervisor.CloudInit{}, fmt.Errorf("render the master's seed: %w", err)
			}
			return managedcluster.Seed(l.master, userData.Value()), nil
		},
	})
	if err != nil {
		return fmt.Errorf("launch the master: %w", err)
	}
	launched[l.master.InstanceID] = master

	// 2. The endpoint record, so <cluster>.<vpc> resolves on the VPC (S7).
	if err := managedcluster.PublishEndpoint(ctx, s.hv, l.vpc.LibvirtNetwork, l.master.PrivateIP, l.endpoint); err != nil {
		return err
	}

	// 3. The master answers ALIVE.
	api, err := s.api(s.connection(l))
	if err != nil {
		return fmt.Errorf("open the status of cluster %q: %w", row.ID, err)
	}
	defer api.Close()
	var lastErr error
	if err := s.await(ctx, row.ID, "the master to be alive", func(ctx context.Context) bool {
		st, err := api.Status(ctx)
		lastErr = err
		return err == nil && st.Alive()
	}); err != nil {
		if lastErr != nil {
			return fmt.Errorf("%w; the master last answered: %v", err, lastErr) //nolint:errorlint // one cause is wrapped, the other is context
		}
		return err
	}

	// 4. The workers, each offering its VM's vCPUs and its memory less the
	//    OS's share.
	for _, w := range l.workers {
		req, err := s.workerRequest(ctx, l, w, secret)
		if err != nil {
			return err
		}
		got, err := s.instances.LaunchManaged(ctx, w.InstanceID, instance.ManagedLaunch{
			Seed: func(context.Context) (hypervisor.CloudInit, error) {
				userData, err := sparkcloudinit.WorkerUserData(req)
				if err != nil {
					return hypervisor.CloudInit{}, fmt.Errorf("render the seed of %s: %w", w.Name, err)
				}
				return managedcluster.Seed(w, userData.Value()), nil
			},
		})
		if err != nil {
			return fmt.Errorf("launch worker %s: %w", w.Name, err)
		}
		launched[w.InstanceID] = got
	}

	// 5. Every worker registered. Past the timeout the cluster is published
	//    anyway, degraded, because its master works.
	var missing []string
	waitErr := s.await(ctx, row.ID, "every worker to register", func(ctx context.Context) bool {
		missing = s.unregistered(ctx, l, api)
		return len(missing) == 0
	})
	if waitErr != nil && !errors.Is(waitErr, managedcluster.ErrTimedOut) {
		return waitErr
	}

	// 6. One transaction publishes the VMs and the cluster together.
	endpoint := sparkcloudinit.MasterURL(l.endpoint)
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for instanceID, got := range launched {
			if err := s.instances.MarkManagedRunningInTx(ctx, tx, instanceID, got); err != nil {
				return err
			}
		}
		if _, err := tx.MarkSparkClusterActive(ctx, sqlc.MarkSparkClusterActiveParams{
			ID: row.ID, Endpoint: nullable(endpoint), UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark cluster %q active: %w", row.ID, err)
		}
		if len(missing) > 0 {
			_, err := tx.SetSparkClusterState(ctx, sqlc.SetSparkClusterStateParams{
				ID: row.ID, State: string(StateDegraded), UpdatedAt: formatTime(s.now()),
				StateReason: nullable("workers not registered after the create: " + strings.Join(missing, ", ")),
			})
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "spark: create finished", "cluster_id", row.ID, "endpoint", endpoint,
		"unregistered", len(missing))
	return nil
}

// workerRequest is a worker's seed request: its instance type sizes it.
func (s *Service) workerRequest(ctx context.Context, l layout, w managedcluster.Node, secret sparkcloudinit.Secret) (sparkcloudinit.WorkerRequest, error) {
	preset, err := s.store.GetInstanceType(ctx, sqlc.GetInstanceTypeParams{ID: l.types[w.Name]})
	if err != nil {
		return sparkcloudinit.WorkerRequest{}, fmt.Errorf("read the instance type of %s: %w", w.Name, err)
	}
	return sparkcloudinit.WorkerRequest{
		NodeName: w.Name, NodeIP: w.PrivateIP, MasterIP: l.master.PrivateIP, Secret: secret,
		Cores: int(preset.Vcpus), MemoryMiB: max(int(preset.MemoryMib)-osReserveMiB, sparkcloudinit.MinWorkerMemoryMiB),
		DrainTimeout: s.drainTimeout,
	}, nil
}

// unregistered asks the master which expected workers are not ALIVE, and
// records what it saw for the API's Node.alive.
func (s *Service) unregistered(ctx context.Context, l layout, api StatusAPI) []string {
	var names []string
	for _, w := range l.workers {
		names = append(names, w.Name)
	}
	st, err := api.Status(ctx)
	if err != nil {
		return names
	}
	s.health.RecordNodes(l.row.ID, aliveByName(l, st))
	alive := st.AliveWorkers()
	var missing []string
	for _, w := range l.workers {
		if !alive[w.PrivateIP] {
			missing = append(missing, w.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

func aliveByName(l layout, st sparkapi.Status) map[string]bool {
	alive := st.AliveWorkers()
	out := map[string]bool{l.master.Name: st.Alive()}
	for _, w := range l.workers {
		out[w.Name] = alive[w.PrivateIP]
	}
	return out
}

// await polls cond until it holds, the cluster is being deleted, or the ready
// timeout runs out.
func (s *Service) await(ctx context.Context, clusterID, what string, cond func(context.Context) bool) error {
	return managedcluster.Wait{
		Timeout: s.readyTimeout, Interval: s.pollInterval, Canceled: ErrCreateCancelled,
		Deleting: func(ctx context.Context) bool {
			row, err := s.store.GetSparkCluster(ctx, sqlc.GetSparkClusterParams{ID: clusterID})
			return err == nil && State(row.State) == StateDeleting
		},
	}.For(ctx, clusterID, what, cond)
}

func (s *Service) setState(ctx context.Context, clusterID string, state State, reason string) {
	if _, err := s.store.SetSparkClusterState(ctx, sqlc.SetSparkClusterStateParams{
		ID: clusterID, State: string(state), StateReason: nullable(reason), UpdatedAt: formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "spark: could not record the cluster's state",
			"cluster_id", clusterID, "state", state, "error", err)
		return
	}
	s.logger.InfoContext(ctx, "spark: cluster state", "cluster_id", clusterID, "state", state, "reason", reason)
}

// Delete queues spark.delete. It is allowed from every state but deleting:
// delete is the way out of a create that failed or is stuck.
func (s *Service) Delete(ctx context.Context, clusterID string) (Cluster, error) {
	current, err := s.Get(ctx, clusterID)
	if err != nil {
		return Cluster{}, err
	}
	if current.State == StateDeleting {
		return Cluster{}, fmt.Errorf("%w: cluster %q is already being deleted", service.ErrConflict, clusterID)
	}
	var taskID string
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if _, err := tx.SetSparkClusterState(ctx, sqlc.SetSparkClusterStateParams{
			ID: clusterID, State: string(StateDeleting), UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark cluster %q deleting: %w", clusterID, err)
		}
		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeSparkClusterDelete, clusterID,
			managedcluster.Payload{ClusterID: clusterID})
		if err != nil {
			return err
		}
		taskID = queued.ID
		return nil
	})
	if err != nil {
		return Cluster{}, err
	}
	current.State, current.StateReason, current.TaskID = StateDeleting, nil, &taskID
	s.logger.InfoContext(ctx, "spark: delete queued", "cluster_id", clusterID, "task_id", taskID)
	return current, nil
}

// DeleteHandler runs spark.delete: every node's VM, the endpoint record, the
// access key, the rows, and the secrets directory last. No decommission: the
// whole cluster is going. The buckets and their event logs are the user's
// and stay. Every step tolerates what is already gone.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
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
			return s.removeSecrets(ctx, clusterID)
		}
		return fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	l, err := s.layout(ctx, row)
	if err != nil {
		return err
	}
	nodes := append([]managedcluster.Node{l.master}, l.workers...)
	if err := managedcluster.Teardown(ctx, s.tear, nodes); err != nil {
		return err
	}
	if err := managedcluster.RemoveEndpoint(ctx, s.hv, l.vpc.LibvirtNetwork, l.master.PrivateIP, l.endpoint); err != nil {
		return err
	}
	// The key before the rows: its id is on the row, and a retry after a
	// failure here must still know which key to remove.
	if row.AccessKeyID.Valid {
		if s.objects == nil {
			s.logger.ErrorContext(ctx, "spark: object storage is off, so the cluster's access key was not removed; "+
				"remove it from RustFS by hand", "cluster_id", clusterID, "access_key", row.AccessKeyID.String)
		} else if err := s.objects.DeleteAccessKey(ctx, row.AccessKeyID.String); err != nil {
			return fmt.Errorf("remove the cluster's access key: %w", err)
		}
	}
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for _, n := range nodes {
			if n.InstanceID == "" {
				continue
			}
			if err := tx.DeleteSparkNode(ctx, sqlc.DeleteSparkNodeParams{InstanceID: n.InstanceID}); err != nil {
				return fmt.Errorf("delete the node row of %s: %w", n.Name, err)
			}
			if err := s.instances.DeleteManagedInTx(ctx, tx, n.InstanceID); err != nil {
				return err
			}
		}
		return tx.DeleteSparkCluster(ctx, sqlc.DeleteSparkClusterParams{ID: clusterID})
	}); err != nil {
		return err
	}
	s.health.Forget(clusterID)
	if err := s.removeSecrets(ctx, clusterID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "spark: cluster deleted", "cluster_id", clusterID, "nodes", len(nodes))
	return nil
}

// tear removes one of the cluster's VMs, tolerating what is already gone.
func (s *Service) tear(ctx context.Context, instanceID string) error {
	return s.instances.TeardownManaged(ctx, instanceID, instance.ManagedBySpark)
}
