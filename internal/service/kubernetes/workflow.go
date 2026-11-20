package kubernetes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
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

// workerTokenTTL is how long the token the workers of a create join with
// lives. It is minted once the API answers, so it only has to outlast their
// boot.
const workerTokenTTL = time.Hour

func decodePayload(t worker.Task) (string, error) {
	return managedcluster.DecodePayload(t, ErrBadTaskPayload)
}

// layout is what both tasks read before they act.
type layout struct {
	row          sqlc.K8sCluster
	vpc          sqlc.Vpc
	controlPlane managedcluster.Node
	workers      []managedcluster.Node
	endpoint     string
}

func (s *Service) layout(ctx context.Context, row sqlc.K8sCluster) (layout, error) {
	vpc, err := s.store.GetVPC(ctx, sqlc.GetVPCParams{ID: row.VpcID})
	if err != nil {
		return layout{}, fmt.Errorf("read the VPC of cluster %q: %w", row.ID, err)
	}
	nodes, err := s.store.ListK8sNodes(ctx, sqlc.ListK8sNodesParams{ClusterID: row.ID})
	if err != nil {
		return layout{}, fmt.Errorf("list the nodes of cluster %q: %w", row.ID, err)
	}
	out := layout{row: row, vpc: vpc, endpoint: endpointName(row.Name, vpc.Name)}
	for _, n := range nodes {
		node := managedcluster.Node{InstanceID: n.InstanceID, Name: n.Name, Role: n.Role, PrivateIP: n.PrivateIp.String}
		if n.Role == RoleControlPlane {
			out.controlPlane = node
		} else {
			out.workers = append(out.workers, node)
		}
	}
	return out, nil
}

func (l layout) nodeNames() []string {
	names := []string{l.controlPlane.Name}
	for _, w := range l.workers {
		names = append(names, w.Name)
	}
	return names
}

// CreateHandler runs k8s.create (section 4): the control plane, its endpoint
// record, the API, the workers, until every node is Ready.
//
// A control plane that never answers is left as it is, and the cluster is
// failed: the VMs are fine, their consoles are the evidence, and delete is the
// way out -- as for a database whose engine never answered. Nodes that never
// become Ready make the cluster degraded, not failed: the control plane works.
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

	row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil // deleted before the task ran
		}
		return fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	switch State(row.State) {
	case StateCreating:
	default:
		// Already finished, failed, or being deleted: a requeued task must not
		// build anything twice or rebuild what a delete is removing.
		s.logger.InfoContext(ctx, "kubernetes: create skipped", "cluster_id", row.ID, "state", row.State)
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

func (s *Service) create(ctx context.Context, row sqlc.K8sCluster) error {
	l, err := s.layout(ctx, row)
	if err != nil {
		return err
	}
	pki, err := k8spki.LoadDir(s.pkiDir(row.ID))
	if err != nil {
		return fmt.Errorf("read the PKI of cluster %q: %w", row.ID, err)
	}

	// 1. The control plane. Its seed is rendered only if no earlier attempt
	//    wrote one: LaunchManaged adopts an existing seed volume, so the
	//    token generated here is never needed twice.
	launched := map[string]instance.ManagedLaunched{}
	cp, err := s.instances.LaunchManaged(ctx, l.controlPlane.InstanceID, instance.ManagedLaunch{
		Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
			token, err := k8spki.NewBootstrapToken(nil)
			if err != nil {
				return hypervisor.CloudInit{}, err
			}
			userData, err := k8scloudinit.ControlPlaneUserData(k8scloudinit.ControlPlaneRequest{
				ClusterName: row.Name, NodeName: l.controlPlane.Name, Endpoint: l.endpoint,
				NodeIP: l.controlPlane.PrivateIP, PodCIDR: row.PodCidr, ServiceCIDR: row.ServiceCidr,
				Token: token, PKI: pki,
			})
			if err != nil {
				return hypervisor.CloudInit{}, fmt.Errorf("render the control plane's seed: %w", err)
			}
			return managedcluster.Seed(l.controlPlane, userData), nil
		},
	})
	if err != nil {
		return fmt.Errorf("launch the control plane: %w", err)
	}
	launched[l.controlPlane.InstanceID] = cp

	// 2. The endpoint record, so api.<cluster>.<vpc> resolves on the VPC (K6).
	if err := managedcluster.PublishEndpoint(ctx, s.hv, l.vpc.LibvirtNetwork, l.controlPlane.PrivateIP, l.endpoint); err != nil {
		return err
	}

	// 3. The API server.
	api, err := s.api(s.connection(l, pki))
	if err != nil {
		return fmt.Errorf("open the API of cluster %q: %w", row.ID, err)
	}
	defer api.Close()

	// 4. The workers, with a token the API mints now: whatever attempt this
	//    is, the token is fresh and the API is the one that will check it.
	//
	//    /readyz answers before the seed has applied the RBAC that admits
	//    kumo:system -- it can only be applied once the API is up -- and on a
	//    fast node the gap is seconds (M8.8 hit it). So the mint is part of the
	//    wait: a refusal or an unreachable API is "not yet", and anything else
	//    fails the create at once.
	var token k8spki.BootstrapToken
	var lastRefusal, fatal error
	if err := s.await(ctx, row.ID, "the API server to admit kumod", func(ctx context.Context) bool {
		if api.Ready(ctx) != nil {
			return false
		}
		if len(l.workers) == 0 {
			return true
		}
		fresh, err := k8spki.NewBootstrapToken(nil)
		if err != nil {
			fatal = err
			return true
		}
		err = api.CreateBootstrapToken(ctx, fresh, s.now().Add(workerTokenTTL))
		switch {
		case err == nil:
			token = fresh
			return true
		case errors.Is(err, k8sapi.ErrForbidden), errors.Is(err, k8sapi.ErrUnavailable):
			lastRefusal = err
			return false
		default:
			fatal = err
			return true
		}
	}); err != nil {
		if lastRefusal != nil {
			return fmt.Errorf("%w; the API server last answered: %v", err, lastRefusal) //nolint:errorlint // one cause is wrapped, the other is context
		}
		return err
	}
	if fatal != nil {
		return fmt.Errorf("mint the workers' bootstrap token: %w", fatal)
	}

	if len(l.workers) > 0 {
		hash, err := k8spki.CAHash(pki.CA.CertPEM)
		if err != nil {
			return err
		}
		for _, w := range l.workers {
			got, err := s.instances.LaunchManaged(ctx, w.InstanceID, instance.ManagedLaunch{
				Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
					userData, err := k8scloudinit.WorkerUserData(k8scloudinit.WorkerRequest{
						NodeName: w.Name, Endpoint: l.endpoint, ControlPlaneIP: l.controlPlane.PrivateIP,
						NodeIP: w.PrivateIP, Token: token, CAHash: hash,
					})
					if err != nil {
						return hypervisor.CloudInit{}, fmt.Errorf("render the seed of %s: %w", w.Name, err)
					}
					return managedcluster.Seed(w, userData), nil
				},
			})
			if err != nil {
				return fmt.Errorf("launch worker %s: %w", w.Name, err)
			}
			launched[w.InstanceID] = got
		}
	}

	// 5. Every node Ready. Past the timeout the cluster is published anyway,
	//    degraded, because its control plane works (section 4).
	var notReady []string
	waitErr := s.await(ctx, row.ID, "every node to be Ready", func(ctx context.Context) bool {
		notReady = s.unreadyNodes(ctx, row.ID, api, l.nodeNames())
		return len(notReady) == 0
	})
	if waitErr != nil && !errors.Is(waitErr, managedcluster.ErrTimedOut) {
		return waitErr
	}

	// 6. One transaction publishes the VMs and the cluster together.
	endpoint := endpointURL(l.endpoint)
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for instanceID, got := range launched {
			if err := s.instances.MarkManagedRunningInTx(ctx, tx, instanceID, got); err != nil {
				return err
			}
		}
		if _, err := tx.MarkK8sClusterActive(ctx, sqlc.MarkK8sClusterActiveParams{
			ID: row.ID, Endpoint: nullable(endpoint), UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark cluster %q active: %w", row.ID, err)
		}
		if len(notReady) > 0 {
			_, err := tx.SetK8sClusterState(ctx, sqlc.SetK8sClusterStateParams{
				ID: row.ID, State: string(StateDegraded), UpdatedAt: formatTime(s.now()),
				StateReason: nullable("nodes not Ready after the create: " + strings.Join(notReady, ", ")),
			})
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "kubernetes: create finished",
		"cluster_id", row.ID, "endpoint", endpoint, "not_ready", len(notReady))
	return nil
}

func (s *Service) connection(l layout, pki *k8spki.ClusterPKI) Connection {
	return Connection{
		ClusterID: l.row.ID, Address: net.JoinHostPort(l.controlPlane.PrivateIP, "6443"),
		ServerName: l.endpoint, PKI: pki, Nodes: l.nodeNames(),
	}
}

// unreadyNodes asks the API which expected nodes are not Ready, and records
// what it saw for the API's Node.ready.
func (s *Service) unreadyNodes(ctx context.Context, clusterID string, api ClusterAPI, expected []string) []string {
	nodes, err := api.Nodes(ctx)
	if err != nil {
		return expected
	}
	ready := map[string]bool{}
	for _, n := range nodes {
		ready[n.Name] = n.Ready
	}
	s.health.RecordNodes(clusterID, ready)
	var missing []string
	for _, name := range expected {
		if !ready[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// await polls cond until it holds, the cluster is being deleted, or the ready
// timeout runs out.
func (s *Service) await(ctx context.Context, clusterID, what string, cond func(context.Context) bool) error {
	return managedcluster.Wait{
		Timeout: s.readyTimeout, Interval: s.pollInterval, Canceled: ErrCreateCancelled,
		Deleting: func(ctx context.Context) bool {
			row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
			return err == nil && State(row.State) == StateDeleting
		},
	}.For(ctx, clusterID, what, cond)
}

func (s *Service) setState(ctx context.Context, clusterID string, state State, reason string) {
	if _, err := s.store.SetK8sClusterState(ctx, sqlc.SetK8sClusterStateParams{
		ID: clusterID, State: string(state), StateReason: nullable(reason), UpdatedAt: formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "kubernetes: could not record the cluster's state",
			"cluster_id", clusterID, "state", state, "error", err)
		return
	}
	s.logger.InfoContext(ctx, "kubernetes: cluster state", "cluster_id", clusterID, "state", state, "reason", reason)
}

// Delete queues k8s.delete. It is allowed from every state but deleting:
// delete is the way out of a create that failed or is stuck (section 4).
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
		if _, err := tx.SetK8sClusterState(ctx, sqlc.SetK8sClusterStateParams{
			ID: clusterID, State: string(StateDeleting), UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("mark cluster %q deleting: %w", clusterID, err)
		}
		queued, err := s.tasks.CreateInTx(ctx, tx, task.TypeK8sClusterDelete, clusterID,
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
	current.State = StateDeleting
	current.StateReason = nil
	current.TaskID = &taskID
	s.logger.InfoContext(ctx, "kubernetes: delete queued", "cluster_id", clusterID, "task_id", taskID)
	return current, nil
}

// DeleteHandler runs k8s.delete: every node's VM, the endpoint record, the
// rows, and the PKI directory last, so a delete that fails partway can still
// be retried with a working credential (section 4). No drain: the whole
// cluster is going. Every step tolerates what is already gone.
func (s *Service) DeleteHandler(ctx context.Context, t worker.Task) error {
	clusterID, err := decodePayload(t)
	if err != nil {
		return err
	}
	// A create waiting on this cluster notices the deleting state and lets go
	// of the lock, so this waits at most one poll interval.
	release, err := s.locks.Acquire(ctx, LockKey(clusterID), t.Type)
	if err != nil {
		return fmt.Errorf("take the lock on cluster %q: %w", clusterID, err)
	}
	defer release()

	row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s.removePKI(ctx, clusterID)
		}
		return fmt.Errorf("get cluster %q: %w", clusterID, err)
	}
	l, err := s.layout(ctx, row)
	if err != nil {
		return err
	}
	nodes := append([]managedcluster.Node{l.controlPlane}, l.workers...)
	if err := managedcluster.Teardown(ctx, s.tear, nodes); err != nil {
		return err
	}
	if err := managedcluster.RemoveEndpoint(ctx, s.hv, l.vpc.LibvirtNetwork, l.controlPlane.PrivateIP, l.endpoint); err != nil {
		return err
	}
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for _, n := range nodes {
			if n.InstanceID == "" {
				continue
			}
			if err := tx.DeleteK8sNode(ctx, sqlc.DeleteK8sNodeParams{InstanceID: n.InstanceID}); err != nil {
				return fmt.Errorf("delete the node row of %s: %w", n.Name, err)
			}
			if err := s.instances.DeleteManagedInTx(ctx, tx, n.InstanceID); err != nil {
				return err
			}
		}
		return tx.DeleteK8sCluster(ctx, sqlc.DeleteK8sClusterParams{ID: clusterID})
	}); err != nil {
		return err
	}
	s.health.Forget(clusterID)
	if err := s.removePKI(ctx, clusterID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "kubernetes: cluster deleted", "cluster_id", clusterID, "nodes", len(nodes))
	return nil
}

// tear removes one of the cluster's VMs, tolerating what is already gone.
func (s *Service) tear(ctx context.Context, instanceID string) error {
	return s.instances.TeardownManaged(ctx, instanceID, instance.ManagedByKubernetes)
}

func (s *Service) removePKI(ctx context.Context, clusterID string) error {
	if err := os.RemoveAll(s.pkiDir(clusterID)); err != nil {
		return fmt.Errorf("remove the PKI of cluster %q: %w", clusterID, err)
	}
	s.logger.DebugContext(ctx, "kubernetes: PKI removed", "cluster_id", clusterID)
	return nil
}

// Make sure the real client satisfies the seam.
var _ ClusterAPI = (*k8sapi.Client)(nil)
