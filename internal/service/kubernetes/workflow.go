package kubernetes

import (
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/ahmedmohamedut/kumo/internal/service/cloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/k8scloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// workerTokenTTL is how long the token the workers of a create join with
// lives. It is minted once the API answers, so it only has to outlast their
// boot.
const workerTokenTTL = time.Hour

// clusterPayload is the input of both tasks: only the cluster's id. Everything
// else lives on the rows, and nothing secret goes into a payload.
type clusterPayload struct {
	ClusterID string `json:"cluster_id"`
}

func decodePayload(t worker.Task) (string, error) {
	var p clusterPayload
	if err := json.Unmarshal(t.Payload, &p); err != nil || p.ClusterID == "" {
		return "", fmt.Errorf("%w: task %s", ErrBadTaskPayload, t.ID)
	}
	return p.ClusterID, nil
}

// clusterNode is a node row with the instance it is.
type clusterNode struct {
	instanceID string
	name       string
	role       string
	privateIP  string
}

// layout is what both tasks read before they act.
type layout struct {
	row          sqlc.K8sCluster
	vpc          sqlc.Vpc
	controlPlane clusterNode
	workers      []clusterNode
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
		node := clusterNode{instanceID: n.InstanceID, name: n.Name, role: n.Role, privateIP: n.PrivateIp.String}
		if n.Role == RoleControlPlane {
			out.controlPlane = node
		} else {
			out.workers = append(out.workers, node)
		}
	}
	return out, nil
}

func (l layout) nodeNames() []string {
	names := []string{l.controlPlane.name}
	for _, w := range l.workers {
		names = append(names, w.name)
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
	cp, err := s.instances.LaunchManaged(ctx, l.controlPlane.instanceID, instance.ManagedLaunch{
		Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
			token, err := k8spki.NewBootstrapToken(nil)
			if err != nil {
				return hypervisor.CloudInit{}, err
			}
			userData, err := k8scloudinit.ControlPlaneUserData(k8scloudinit.ControlPlaneRequest{
				ClusterName: row.Name, NodeName: l.controlPlane.name, Endpoint: l.endpoint,
				NodeIP: l.controlPlane.privateIP, PodCIDR: row.PodCidr, ServiceCIDR: row.ServiceCidr,
				Token: token, PKI: pki,
			})
			if err != nil {
				return hypervisor.CloudInit{}, fmt.Errorf("render the control plane's seed: %w", err)
			}
			return seed(l.controlPlane, userData), nil
		},
	})
	if err != nil {
		return fmt.Errorf("launch the control plane: %w", err)
	}
	launched[l.controlPlane.instanceID] = cp

	// 2. The endpoint record, so api.<cluster>.<vpc> resolves on the VPC (K6).
	if err := s.hv.AddDNSHost(ctx, l.vpc.LibvirtNetwork, hypervisor.DNSHost{
		IP: l.controlPlane.privateIP, Hostnames: []string{l.endpoint},
	}); err != nil && !errors.Is(err, hypervisor.ErrAlreadyExists) {
		return fmt.Errorf("publish the endpoint %s: %w", l.endpoint, err)
	}

	// 3. The API server.
	api, err := s.api(s.connection(l, pki))
	if err != nil {
		return fmt.Errorf("open the API of cluster %q: %w", row.ID, err)
	}
	defer api.Close()
	if err := s.await(ctx, row.ID, "the API server to be ready", func(ctx context.Context) bool {
		return api.Ready(ctx) == nil
	}); err != nil {
		return err
	}

	// 4. The workers, with a token the API mints now: whatever attempt this
	//    is, the token is fresh and the API is the one that will check it.
	if len(l.workers) > 0 {
		token, err := k8spki.NewBootstrapToken(nil)
		if err != nil {
			return err
		}
		if err := api.CreateBootstrapToken(ctx, token, s.now().Add(workerTokenTTL)); err != nil {
			return fmt.Errorf("mint the workers' bootstrap token: %w", err)
		}
		hash, err := k8spki.CAHash(pki.CA.CertPEM)
		if err != nil {
			return err
		}
		for _, w := range l.workers {
			got, err := s.instances.LaunchManaged(ctx, w.instanceID, instance.ManagedLaunch{
				Seed: func(ctx context.Context) (hypervisor.CloudInit, error) {
					userData, err := k8scloudinit.WorkerUserData(k8scloudinit.WorkerRequest{
						NodeName: w.name, Endpoint: l.endpoint, ControlPlaneIP: l.controlPlane.privateIP,
						NodeIP: w.privateIP, Token: token, CAHash: hash,
					})
					if err != nil {
						return hypervisor.CloudInit{}, fmt.Errorf("render the seed of %s: %w", w.name, err)
					}
					return seed(w, userData), nil
				},
			})
			if err != nil {
				return fmt.Errorf("launch worker %s: %w", w.name, err)
			}
			launched[w.instanceID] = got
		}
	}

	// 5. Every node Ready. Past the timeout the cluster is published anyway,
	//    degraded, because its control plane works (section 4).
	var notReady []string
	waitErr := s.await(ctx, row.ID, "every node to be Ready", func(ctx context.Context) bool {
		notReady = s.unreadyNodes(ctx, row.ID, api, l.nodeNames())
		return len(notReady) == 0
	})
	if waitErr != nil && !errors.Is(waitErr, errTimedOut) {
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

func seed(n clusterNode, userData string) hypervisor.CloudInit {
	return hypervisor.CloudInit{
		MetaData:      cloudinit.MetaData(n.instanceID, n.name),
		UserData:      userData,
		NetworkConfig: cloudinit.NetworkConfig(),
	}
}

func (s *Service) connection(l layout, pki *k8spki.ClusterPKI) Connection {
	return Connection{
		ClusterID: l.row.ID, Address: net.JoinHostPort(l.controlPlane.privateIP, "6443"),
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
	s.recordReady(clusterID, ready)
	var missing []string
	for _, name := range expected {
		if !ready[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

var errTimedOut = errors.New("kubernetes: timed out")

// await polls cond until it holds, the cluster is being deleted, or the ready
// timeout runs out.
func (s *Service) await(ctx context.Context, clusterID, what string, cond func(context.Context) bool) error {
	deadline := time.Now().Add(s.readyTimeout)
	for {
		if cond(ctx) {
			return nil
		}
		if row, err := s.store.GetK8sCluster(ctx, sqlc.GetK8sClusterParams{ID: clusterID}); err == nil &&
			State(row.State) == StateDeleting {
			return fmt.Errorf("%w (cluster %s)", ErrCreateCancelled, clusterID)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w waiting for %s after %s", errTimedOut, what, s.readyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.pollInterval):
		}
	}
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
			clusterPayload{ClusterID: clusterID})
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
	nodes := append([]clusterNode{l.controlPlane}, l.workers...)
	for _, n := range nodes {
		if n.instanceID == "" {
			continue
		}
		if err := s.instances.TeardownManaged(ctx, n.instanceID, instance.ManagedByKubernetes); err != nil {
			return fmt.Errorf("tear down node %s: %w", n.name, err)
		}
	}
	if l.controlPlane.privateIP != "" {
		if err := s.hv.DeleteDNSHost(ctx, l.vpc.LibvirtNetwork, hypervisor.DNSHost{
			IP: l.controlPlane.privateIP, Hostnames: []string{l.endpoint},
		}); err != nil && !errors.Is(err, hypervisor.ErrNotFound) {
			return fmt.Errorf("remove the endpoint %s: %w", l.endpoint, err)
		}
	}
	if err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		for _, n := range nodes {
			if n.instanceID == "" {
				continue
			}
			if err := tx.DeleteK8sNode(ctx, sqlc.DeleteK8sNodeParams{InstanceID: n.instanceID}); err != nil {
				return fmt.Errorf("delete the node row of %s: %w", n.name, err)
			}
			if err := s.instances.DeleteManagedInTx(ctx, tx, n.instanceID); err != nil {
				return err
			}
		}
		return tx.DeleteK8sCluster(ctx, sqlc.DeleteK8sClusterParams{ID: clusterID})
	}); err != nil {
		return err
	}
	s.forget(clusterID)
	if err := s.removePKI(ctx, clusterID); err != nil {
		return err
	}
	s.logger.InfoContext(ctx, "kubernetes: cluster deleted", "cluster_id", clusterID, "nodes", len(nodes))
	return nil
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
