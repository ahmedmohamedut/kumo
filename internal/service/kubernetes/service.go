// Package kubernetes owns Kumo's Kubernetes clusters
// (docs/design/08-kubernetes.md): a control plane and one worker node group,
// created, watched, and deleted on the user's behalf, with the VMs underneath
// hidden from the Instances pages.
//
// It is the databases module's shape (internal/service/dbinstance): the VMs are
// managed rows reserved and launched through internal/service/instance, the
// seeds come from internal/service/k8scloudinit with a PKI from
// internal/service/k8spki, and the cluster's own API is reached through a
// ClusterAPI, internal/k8sapi on a real node.
package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// Defaults of the module's settings (section 2).
const (
	DefaultReadyTimeout  = 20 * time.Minute
	DefaultProbeInterval = 30 * time.Second
	// unhealthyPasses is how many health passes in a row a cluster must fail
	// before it is degraded, so one slow answer does not flap the state.
	unhealthyPasses = 3
)

// State is a cluster's lifecycle (section 3; the CHECK on k8s_clusters.state).
type State string

const (
	StateCreating State = "creating"
	StateActive   State = "active"
	StateUpdating State = "updating"
	StateDegraded State = "degraded"
	StateDeleting State = "deleting"
	StateFailed   State = "failed"
)

func knownState(s State) bool {
	switch s {
	case StateCreating, StateActive, StateUpdating, StateDegraded, StateDeleting, StateFailed:
		return true
	}
	return false
}

// Node roles, as k8s_nodes.role records them.
const (
	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"
)

// Fixed by the design rather than chosen per cluster in this version (K9).
const (
	PodCIDR     = "100.64.0.0/16"
	ServiceCIDR = "100.96.0.0/16"
	// DefaultNodeGroup is the one node group a v1 cluster has.
	DefaultNodeGroup = "default"
)

// Sentinel errors.
var (
	// ErrNoEngineImage means no available kubernetes engine image matches. It
	// wraps service.ErrConflict, as the databases module's does, and the API
	// answers it with the no_engine_image code.
	ErrNoEngineImage = fmt.Errorf("%w: no available Kubernetes engine image", service.ErrConflict)
	// ErrCreateCancelled means a delete arrived while a create was running.
	ErrCreateCancelled = errors.New("kubernetes: the cluster is being deleted")
	// ErrBadTaskPayload means a task names no cluster.
	ErrBadTaskPayload = errors.New("kubernetes: malformed task payload")
)

// ClusterAPI is what kumod asks of a cluster's API server (section 6).
// *k8sapi.Client implements it on a real node.
type ClusterAPI interface {
	Ready(ctx context.Context) error
	Nodes(ctx context.Context) ([]k8sapi.Node, error)
	CreateBootstrapToken(ctx context.Context, token k8spki.BootstrapToken, expires time.Time) error
	Cordon(ctx context.Context, node string) error
	PodsOnNode(ctx context.Context, node string) ([]k8sapi.Pod, error)
	Evict(ctx context.Context, namespace, pod string) error
	DeleteNode(ctx context.Context, node string) error
	Close()
}

// Connection is how kumod reaches one cluster.
type Connection struct {
	ClusterID string
	// Address is the control plane's address and the API port.
	Address string
	// ServerName is the endpoint the API server's certificate names.
	ServerName string
	PKI        *k8spki.ClusterPKI
	// Nodes is the node names kumod expects, for a fake that has no cluster
	// to ask.
	Nodes []string
}

// APIFactory opens a ClusterAPI for a cluster. A seam, so a node on the fake
// hypervisor -- which has no guests, so no API server ever answers -- can be
// given one that does (FakeAPI), the way the databases module gets a probe
// that answers (internal/cli/serve.go).
type APIFactory func(conn Connection) (ClusterAPI, error)

// RealAPI opens internal/k8sapi with kumod's kumo:system identity.
func RealAPI(conn Connection) (ClusterAPI, error) {
	return k8sapi.New(k8sapi.Config{
		Address:    conn.Address,
		ServerName: conn.ServerName,
		CA:         conn.PKI.CA.CertPEM,
		ClientCert: conn.PKI.System.CertPEM,
		ClientKey:  conn.PKI.System.KeyPEM,
	})
}

// Options configures the service.
type Options struct {
	Logger     *slog.Logger
	Store      *store.Store
	Hypervisor hypervisor.Hypervisor
	Instances  *instance.Service
	Tasks      *task.Service
	// DataDir is $KUMO_DATA_DIR; each cluster's PKI lives in
	// <DataDir>/clusters/<id>/ (section 5).
	DataDir string
	// API opens a cluster's API; RealAPI when nil.
	API APIFactory
	// ReadyTimeout and ProbeInterval come from KUMO_K8S_READY_TIMEOUT and
	// KUMO_K8S_PROBE_INTERVAL.
	ReadyTimeout  time.Duration
	ProbeInterval time.Duration
	// PollInterval is how often a create polls a starting cluster; tests
	// shorten it.
	PollInterval time.Duration
	// DrainTimeout bounds the drain of one node in a scale down;
	// DefaultDrainTimeout when zero.
	DrainTimeout time.Duration
	Locks        *locks.Set
	Now          func() time.Time
}

// Service owns Kubernetes clusters.
type Service struct {
	logger        *slog.Logger
	store         *store.Store
	hv            hypervisor.Hypervisor
	instances     *instance.Service
	tasks         *task.Service
	dataDir       string
	api           APIFactory
	readyTimeout  time.Duration
	probeInterval time.Duration
	pollInterval  time.Duration
	drainTimeout  time.Duration
	locks         *locks.Set
	now           func() time.Time

	mu sync.Mutex
	// probedAt and failures are the health pass's memory (section 6): when a
	// cluster was last asked, and how many passes in a row it failed.
	probedAt map[string]time.Time
	failures map[string]int
	// ready is each node's Ready condition as last seen, by cluster.
	ready map[string]map[string]bool
}

// Constructor errors.
var (
	ErrNoLogger     = errors.New("kubernetes: a logger is required")
	ErrNoStore      = errors.New("kubernetes: a store is required")
	ErrNoHypervisor = errors.New("kubernetes: a hypervisor is required")
	ErrNoInstances  = errors.New("kubernetes: an instance service is required")
	ErrNoTasks      = errors.New("kubernetes: a task service is required")
	ErrNoDataDir    = errors.New("kubernetes: a data directory is required")
)

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Logger == nil:
		return nil, ErrNoLogger
	case opts.Store == nil:
		return nil, ErrNoStore
	case opts.Hypervisor == nil:
		return nil, ErrNoHypervisor
	case opts.Instances == nil:
		return nil, ErrNoInstances
	case opts.Tasks == nil:
		return nil, ErrNoTasks
	case opts.DataDir == "":
		return nil, ErrNoDataDir
	}
	if opts.API == nil {
		opts.API = RealAPI
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = DefaultProbeInterval
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 10 * time.Second
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		logger:        opts.Logger,
		store:         opts.Store,
		hv:            opts.Hypervisor,
		instances:     opts.Instances,
		tasks:         opts.Tasks,
		dataDir:       opts.DataDir,
		api:           opts.API,
		readyTimeout:  opts.ReadyTimeout,
		probeInterval: opts.ProbeInterval,
		pollInterval:  opts.PollInterval,
		drainTimeout:  opts.DrainTimeout,
		locks:         opts.Locks,
		now:           opts.Now,
		probedAt:      map[string]time.Time{},
		failures:      map[string]int{},
		ready:         map[string]map[string]bool{},
	}, nil
}

// LockKey serializes the operations of one cluster.
func LockKey(clusterID string) string { return "k8s-cluster:" + clusterID }

// Handler returns the task types this service answers.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeK8sClusterCreate: worker.HandlerFunc(s.CreateHandler),
		task.TypeK8sClusterDelete: worker.HandlerFunc(s.DeleteHandler),
		task.TypeK8sClusterScale:  worker.HandlerFunc(s.ScaleHandler),
	}
}

// pkiDir is a cluster's PKI directory.
func (s *Service) pkiDir(clusterID string) string {
	return filepath.Join(s.dataDir, "clusters", clusterID)
}

// endpointName is the API server's DNS name, served by the VPC's dnsmasq (K6).
func endpointName(cluster, vpc string) string { return "api." + cluster + "." + vpc }

// endpointURL is what a kubeconfig names, and what the cluster row publishes.
func endpointURL(name string) string { return "https://" + name + ":6443" }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return t
}
