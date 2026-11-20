// Package spark owns Kumo's Spark clusters (docs/design/09-spark.md): a master
// and one worker node group, created, watched and deleted on the user's
// behalf, with the VMs underneath hidden from the Instances pages.
//
// It is the Kubernetes module's shape (internal/service/kubernetes) on the
// machinery they share (internal/service/managedcluster): the VMs are managed
// rows reserved and launched through internal/service/instance, the seeds and
// the client configuration come from internal/service/sparkcloudinit, and the
// master is watched through a StatusAPI, internal/sparkapi on a real node.
package spark

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/managedcluster"
	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/sparkapi"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// Defaults of the module's settings (section 2).
const (
	DefaultReadyTimeout  = 10 * time.Minute
	DefaultProbeInterval = 30 * time.Second
	DefaultDrainTimeout  = 10 * time.Minute
	DefaultS3Region      = "us-east-1"
	// unhealthyPasses is how many health passes in a row a cluster must fail
	// before it is degraded, as for Kubernetes.
	unhealthyPasses = 3
	// osReserveMiB is what a worker keeps for its OS and its daemons; the
	// rest of the VM's memory is offered to executors.
	osReserveMiB = 1024
)

// State is a cluster's lifecycle (section 3; the CHECK on spark_clusters).
type State string

// The cluster states.
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

// Node roles (spark_nodes.role).
const (
	RoleMaster = "master"
	RoleWorker = "worker"
)

// DefaultNodeGroup is the one node group of a v1 cluster.
const DefaultNodeGroup = "default"

var (
	// ErrNoEngineImage means no Spark engine image is available.
	ErrNoEngineImage = fmt.Errorf("%w: no available Spark engine image", service.ErrConflict)
	// ErrCreateCancelled means a delete took the cluster over mid-create.
	ErrCreateCancelled = errors.New("spark: the cluster is being deleted")
	// ErrBadTaskPayload means a task's payload names no cluster.
	ErrBadTaskPayload = errors.New("spark: malformed task payload")
)

// StatusAPI is what the module asks a master: its status. internal/sparkapi
// on a real node; FakeAPI on the fake hypervisor.
type StatusAPI interface {
	Status(ctx context.Context) (sparkapi.Status, error)
	Close()
}

// Connection is what a StatusAPI is built from.
type Connection struct {
	ClusterID string
	// MasterIP is the master's VPC address; the status is on its web UI port.
	MasterIP string
	// WorkerIPs are the workers the cluster should have, which FakeAPI
	// reports as registered.
	WorkerIPs []string
}

// APIFactory builds the StatusAPI of one cluster.
type APIFactory func(conn Connection) (StatusAPI, error)

// RealAPI reads the master through internal/sparkapi.
func RealAPI(conn Connection) (StatusAPI, error) {
	return sparkapi.New(sparkapi.Config{
		Address: net.JoinHostPort(conn.MasterIP, strconv.Itoa(sparkcloudinit.MasterUIPort)),
	})
}

// FakeAPI is the StatusAPI of a node on the fake hypervisor, where no guest
// runs: the master is alive and every expected worker registered.
func FakeAPI(conn Connection) (StatusAPI, error) { return fakeAPI{conn: conn}, nil }

type fakeAPI struct{ conn Connection }

func (f fakeAPI) Status(context.Context) (sparkapi.Status, error) {
	st := sparkapi.Status{Status: sparkapi.MasterAlive, Workers: []sparkapi.Worker{}}
	for _, ip := range f.conn.WorkerIPs {
		st.Workers = append(st.Workers, sparkapi.Worker{Host: ip, State: sparkapi.WorkerAlive})
	}
	return st, nil
}
func (fakeAPI) Close() {}

// Options configures the service.
type Options struct {
	Logger     *slog.Logger
	Store      *store.Store
	Hypervisor hypervisor.Hypervisor
	Instances  *instance.Service
	Tasks      *task.Service
	// ObjectStore is nil with object storage off; a cluster then cannot ask
	// for it.
	ObjectStore objectstore.ObjectStore
	// S3Endpoint is where the guests reach RustFS (KUMO_SPARK_S3_ENDPOINT),
	// S3Region what they sign with.
	S3Endpoint string
	S3Region   string
	// DataDir holds each cluster's secrets under spark/<id>/.
	DataDir string
	API     APIFactory
	// ReadyTimeout bounds each wait of a create; ProbeInterval spaces the
	// health pass's reads of one cluster; PollInterval spaces a wait's reads.
	ReadyTimeout  time.Duration
	ProbeInterval time.Duration
	PollInterval  time.Duration
	// DrainTimeout bounds a worker's decommission (S13), written into its seed.
	DrainTimeout time.Duration
	Locks        *locks.Set
	Now          func() time.Time
}

// Service owns Spark clusters.
type Service struct {
	logger       *slog.Logger
	store        *store.Store
	hv           hypervisor.Hypervisor
	instances    *instance.Service
	tasks        *task.Service
	objects      objectstore.ObjectStore
	s3Endpoint   string
	s3Region     string
	dataDir      string
	api          APIFactory
	readyTimeout time.Duration
	pollInterval time.Duration
	drainTimeout time.Duration
	locks        *locks.Set
	now          func() time.Time
	health       *managedcluster.Health
}

// Constructor errors.
var (
	ErrNoLogger     = errors.New("spark: a logger is required")
	ErrNoStore      = errors.New("spark: a store is required")
	ErrNoHypervisor = errors.New("spark: a hypervisor is required")
	ErrNoInstances  = errors.New("spark: an instance service is required")
	ErrNoTasks      = errors.New("spark: a task service is required")
	ErrNoDataDir    = errors.New("spark: a data directory is required")
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
		opts.PollInterval = 5 * time.Second
	}
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.S3Region == "" {
		opts.S3Region = DefaultS3Region
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Service{
		logger: opts.Logger, store: opts.Store, hv: opts.Hypervisor, instances: opts.Instances,
		tasks: opts.Tasks, objects: opts.ObjectStore, s3Endpoint: opts.S3Endpoint, s3Region: opts.S3Region,
		dataDir: opts.DataDir, api: opts.API, readyTimeout: opts.ReadyTimeout, pollInterval: opts.PollInterval,
		drainTimeout: opts.DrainTimeout, locks: opts.Locks, now: opts.Now,
		health: managedcluster.NewHealth(opts.ProbeInterval, opts.Now),
	}, nil
}

// ObjectStorage reports whether clusters can ask for object storage here.
func (s *Service) ObjectStorage() bool { return s.objects != nil }

// LockKey serializes the operations of one cluster.
func LockKey(clusterID string) string { return "spark-cluster:" + clusterID }

// Handler is the module's tasks.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeSparkClusterCreate: worker.HandlerFunc(s.CreateHandler),
		task.TypeSparkClusterDelete: worker.HandlerFunc(s.DeleteHandler),
	}
}

// endpointName is the master's DNS name in its VPC (S7).
func endpointName(cluster, vpc string) string { return cluster + "." + vpc }

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, value)
	return t
}

// The cluster's secrets directory (section 5): the Spark secret and, with
// object storage, the access key's secret, 0600 in a 0700 directory, outside
// the database.
const (
	sparkSecretFile = "spark.secret"
	s3SecretFile    = "s3.secret"
)

func (s *Service) secretsDir(clusterID string) string {
	return filepath.Join(s.dataDir, "spark", clusterID)
}

func (s *Service) writeSecrets(clusterID string, spark, s3 sparkcloudinit.Secret) error {
	dir := s.secretsDir(clusterID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, sparkSecretFile), []byte(spark.Value()), 0o600); err != nil {
		return fmt.Errorf("write the Spark secret: %w", err)
	}
	if s3 != "" {
		if err := os.WriteFile(filepath.Join(dir, s3SecretFile), []byte(s3.Value()), 0o600); err != nil {
			return fmt.Errorf("write the access key's secret: %w", err)
		}
	}
	return nil
}

// secrets reads the cluster's secrets; the S3 one is empty for a cluster
// without object storage.
func (s *Service) secrets(clusterID string) (spark, s3 sparkcloudinit.Secret, err error) {
	dir := s.secretsDir(clusterID)
	raw, err := os.ReadFile(filepath.Join(dir, sparkSecretFile)) //nolint:gosec // our own data directory
	if err != nil {
		return "", "", fmt.Errorf("read the Spark secret of cluster %q: %w", clusterID, err)
	}
	spark = sparkcloudinit.Secret(raw)
	if raw, err := os.ReadFile(filepath.Join(dir, s3SecretFile)); err == nil { //nolint:gosec // our own data directory
		s3 = sparkcloudinit.Secret(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", fmt.Errorf("read the access key's secret of cluster %q: %w", clusterID, err)
	}
	return spark, s3, nil
}

func (s *Service) removeSecrets(ctx context.Context, clusterID string) error {
	if err := os.RemoveAll(s.secretsDir(clusterID)); err != nil {
		return fmt.Errorf("remove the secrets of cluster %q: %w", clusterID, err)
	}
	s.logger.DebugContext(ctx, "spark: secrets removed", "cluster_id", clusterID)
	return nil
}
