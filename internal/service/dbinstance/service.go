// Package dbinstance owns managed Postgres: creating a DB instance, moving it
// through its lifecycle, listing one and reading it back
// (docs/design/07-databases.md).
//
// A DB instance is a Postgres server with a virtual machine underneath it that
// belongs to Kumo rather than to the user. That VM is an ordinary row in
// instances, hidden from every Instances page and reached only through this
// service (R7): capacity accounting, the reconciler, address allocation and the
// launch rollback go on working unchanged, which a table of our own would have
// meant reimplementing.
//
// Two things are deliberately not here. There is no in-guest agent (R3), so
// everything this package knows about a running database it learns from a TCP
// connect (section 6). And there is no password, anywhere: the create request
// is the only place the admin password exists, the verifier derived from it
// travels to the guest in the seed, and neither is ever written to a row, a
// task payload or a log line (R8).
package dbinstance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// State is the lifecycle of a DB instance. The values mirror the CHECK
// constraint on db_instances.state and the DBInstanceState enum of
// api/openapi.yaml.
//
// They are the database's states, not the VM's: a guest that is running while
// the engine inside it has never answered is a DB instance that is `creating`
// or `failed`, never `available` (section 6).
type State string

const (
	// StateCreating means the create task is building the server.
	StateCreating State = "creating"
	// StateAvailable means the engine answered on the VPC address.
	StateAvailable State = "available"
	// StateStarting means the VM is booting after a start.
	StateStarting State = "starting"
	// StateStopping means an ACPI shutdown is in flight.
	StateStopping State = "stopping"
	// StateStopped means the VM is off. The data disk and the endpoint survive.
	StateStopped State = "stopped"
	// StateRebooting means a restart is in flight.
	StateRebooting State = "rebooting"
	// StateDeleting means the delete task is removing everything.
	StateDeleting State = "deleting"
	// StateFailed means the create gave up or the engine stopped answering.
	// StateReason says which. Delete is the only way out.
	StateFailed State = "failed"
)

// Engine is a database engine. One engine in this version, as a column rather
// than an assumption (R1).
type Engine string

// EnginePostgres is PostgreSQL.
const EnginePostgres Engine = "postgres"

// SortOrder selects the order of a DB instance page.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Pagination bounds, the same as every other list endpoint.
const (
	MinPageSize     = 1
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// Bounds of a create request, from docs/design/07-databases.md section 3.
const (
	// MaxIdentifierLength is the longest identifier, which is shorter than a
	// DNS label because it becomes one with room to spare.
	MaxIdentifierLength = 60
	// MaxNameLength bounds the admin role and the initial database, which are
	// Postgres identifiers.
	MaxNameLength = 63
	// MinStorageGB and MaxStorageGB bound the data disk.
	MinStorageGB = 5
	MaxStorageGB = 2048
	// DefaultPort is the port every DB instance listens on. It is not a
	// request field in this version.
	DefaultPort = 5432
)

// Defaults of the readiness probe (section 6), overridden from the
// configuration.
const (
	// DefaultProbeInterval is how often a creating or running DB instance is
	// probed.
	DefaultProbeInterval = 10 * time.Second
	// DefaultReadyTimeout is how long a create waits for the engine before the
	// DB instance is failed. A first boot creates a cluster, which is slower
	// than a boot and far faster than this.
	DefaultReadyTimeout = 10 * time.Minute
	// probeDialTimeout bounds one connect. It is short because the target is
	// an address on the host's own bridge: anything slower than this is not a
	// database that is merely busy.
	probeDialTimeout = 3 * time.Second
)

// DBInstance is the projection of a db_instances row, in the shape
// api/openapi.yaml promises.
//
// It carries no password, no verifier and no salt, because none of them exists
// to carry (R8). It does carry instance_id: an operator debugging a stuck
// database needs the console log and the orphan report to make sense, and
// hiding the id would make the module harder to support without making it
// safer (section 8).
type DBInstance struct {
	ID            string  `json:"id"`
	Identifier    string  `json:"identifier"`
	State         State   `json:"state"`
	StateReason   *string `json:"state_reason"`
	Engine        Engine  `json:"engine"`
	EngineVersion string  `json:"engine_version"`
	ImageID       string  `json:"image_id"`
	InstanceID    string  `json:"instance_id"`
	InstanceType  string  `json:"instance_type"`
	VCPUs         int     `json:"vcpus"`
	MemoryMiB     int     `json:"memory_mib"`
	StorageGB     int     `json:"storage_gb"`
	Port          int     `json:"port"`
	AdminUsername string  `json:"admin_username"`
	DatabaseName  string  `json:"database_name"`
	// Endpoint is the DNS name clients connect to, written once the guest has
	// an address. It is null while a DB instance is still being created (R9).
	Endpoint *string `json:"endpoint"`
	// TaskID is the task of the most recent state change, so the console can
	// follow a create without guessing which task it was.
	TaskID    *string   `json:"task_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Page is one page of a DB instance listing.
type Page struct {
	Items         []DBInstance `json:"items"`
	NextPageToken *string      `json:"next_page_token"`
}

// Sentinel errors of this service.
var (
	// ErrNoEngineImage means the module is on and no engine image is
	// available, so nothing can be created yet. The api layer answers it with
	// 409 and the no_engine_image code, which is what tells the console to
	// explain the import rather than report a fault (section 8).
	ErrNoEngineImage = fmt.Errorf("%w: no engine image is available on this node",
		service.ErrConflict)
	// ErrBadTaskPayload means a task did not name the DB instance it works on.
	ErrBadTaskPayload = errors.New("dbinstance: the task payload is not acceptable")
	// ErrCredentialGone means a create task has to render the seed but the
	// verifier the request computed is no longer in memory, which is what a
	// restart between the request and the task looks like. Nothing can recover
	// it: the password it came from was discarded when the request was
	// answered (section 5).
	ErrCredentialGone = errors.New(
		"dbinstance: the admin credential of this create is gone; delete the DB instance and create it again")
	// ErrEngineNotReady means the engine never answered inside the timeout.
	ErrEngineNotReady = errors.New("dbinstance: the engine did not answer")
	// ErrCreateCancelled means a delete arrived while the create was waiting
	// for the engine. The delete is the way out of a stuck create, so the
	// create stands down rather than holding the database for the rest of the
	// readiness timeout.
	ErrCreateCancelled = errors.New("dbinstance: the create was canceled by a delete")
)

// Prober reports whether something is listening on a TCP address.
//
// It is a seam so a test does not need a listener, and a function rather than
// an interface because there is exactly one thing to ask. Nothing is sent and
// nothing is parsed: no Postgres driver is added to the binary, and because
// the first-boot script opens the VPC address last, a bare connect already
// implies the role and the database exist (section 6).
type Prober func(ctx context.Context, address string) error

// Options configures the service.
type Options struct {
	// Logger records every state change of a DB instance.
	Logger *slog.Logger
	// Store is the metadata database.
	Store *store.Store
	// Hypervisor creates and deletes the data disk. Everything else about the
	// guest goes through the instance service.
	Hypervisor hypervisor.Hypervisor
	// Instances reserves, launches and runs the VM behind a DB instance.
	Instances *instance.Service
	// Tasks creates the db.create and db.delete tasks.
	Tasks *task.Service
	// InstancesPool is the libvirt pool the data disks live in, beside the
	// root overlays.
	InstancesPool string
	// Locks serializes the operations of one DB instance. A nil value gets a
	// set of its own; it is deliberately not the instance service's set,
	// because the two resources are locked at the same time and sharing one
	// set would make an unrelated instance wait.
	Locks *locks.Set
	// Probe is the readiness probe. Nil means a plain TCP connect.
	Probe Prober
	// ProbeInterval and ReadyTimeout come from KUMO_DB_PROBE_INTERVAL and
	// KUMO_DB_READY_TIMEOUT.
	ProbeInterval time.Duration
	ReadyTimeout  time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Service owns DB instances.
type Service struct {
	logger        *slog.Logger
	store         *store.Store
	hv            hypervisor.Hypervisor
	instances     *instance.Service
	tasks         *task.Service
	instancesPool string
	locks         *locks.Set
	probe         Prober
	probeInterval time.Duration
	readyTimeout  time.Duration
	now           func() time.Time

	// credentials holds the verifier of every create in flight. It is memory
	// and not a row on purpose: a verifier is a password hash, and section 5's
	// promise is that nothing persists it. The cost is that a create
	// interrupted by a restart cannot be resumed, which the create task
	// reports as a failure the operator can act on.
	credentials *credentialStash

	// probedAt is when each DB instance was last probed, so one reconcile pass
	// cannot dial a database more often than the configured interval however
	// often the pass itself runs (section 6, "Rate").
	probedAt sync.Map // db instance id -> time.Time
}

// Sentinel errors of the constructor, so a wiring mistake reads differently
// from a runtime failure.
var (
	ErrNoLogger        = errors.New("dbinstance: a logger is required")
	ErrNoStore         = errors.New("dbinstance: a store is required")
	ErrNoHypervisor    = errors.New("dbinstance: a hypervisor is required")
	ErrNoInstances     = errors.New("dbinstance: an instance service is required")
	ErrNoTasks         = errors.New("dbinstance: a task service is required")
	ErrNoInstancesPool = errors.New("dbinstance: an instances pool name is required")
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
	case opts.InstancesPool == "":
		return nil, ErrNoInstancesPool
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}
	if opts.Probe == nil {
		opts.Probe = dialProbe
	}
	if opts.ProbeInterval <= 0 {
		opts.ProbeInterval = DefaultProbeInterval
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = DefaultReadyTimeout
	}

	return &Service{
		logger:        opts.Logger,
		store:         opts.Store,
		hv:            opts.Hypervisor,
		instances:     opts.Instances,
		tasks:         opts.Tasks,
		instancesPool: opts.InstancesPool,
		locks:         opts.Locks,
		probe:         opts.Probe,
		probeInterval: opts.ProbeInterval,
		readyTimeout:  opts.ReadyTimeout,
		now:           opts.Now,
		credentials:   newCredentialStash(),
	}, nil
}

// LockKey is the key this service serializes the operations of one DB instance
// with. It is exported for the same reason instance.LockKey is: the lock
// belongs to the resource, so anything else acting on it must take the very
// same key.
func LockKey(dbInstanceID string) string { return "db-instance:" + dbInstanceID }

// Handler returns the task types this service answers, for the worker registry.
func (s *Service) Handler() map[string]worker.Handler {
	return map[string]worker.Handler{
		task.TypeDBInstanceCreate: worker.HandlerFunc(s.CreateHandler),
		task.TypeDBInstanceDelete: worker.HandlerFunc(s.DeleteHandler),
	}
}

// dialProbe is the default readiness probe: one TCP connect, nothing written.
func dialProbe(ctx context.Context, address string) error {
	dialer := net.Dialer{Timeout: probeDialTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	return conn.Close()
}

// credentialStash holds the SCRAM verifier of each create in flight.
//
// It is keyed by DB instance id and read by the create task, which is the only
// consumer. A read does not remove the entry, because a task the worker
// requeues inside the same process has to be able to render the seed again;
// forget is called when the create reaches a state it cannot leave by itself.
type credentialStash struct {
	mu sync.Mutex
	by map[string]string
}

func newCredentialStash() *credentialStash {
	return &credentialStash{by: make(map[string]string)}
}

func (c *credentialStash) put(dbInstanceID, verifier string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.by[dbInstanceID] = verifier
}

func (c *credentialStash) get(dbInstanceID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	verifier, ok := c.by[dbInstanceID]
	return verifier, ok
}

func (c *credentialStash) forget(dbInstanceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.by, dbInstanceID)
}

// endpointName is the DNS name a DB instance answers on: the identifier, which
// is the static DHCP host's name, under the VPC's name, which is the DNS domain
// dnsmasq serves for that network (R9).
func endpointName(identifier, vpcName string) string {
	if vpcName == "" {
		return identifier
	}
	return identifier + "." + vpcName
}

// formatTime renders a time the way the schema stores it.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime reads a stored timestamp.
func parseTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

// isUniqueViolation reports a UNIQUE constraint failure. The match is exact
// because a bare "constraint failed" also covers CHECK, NOT NULL and foreign
// key violations, which are not conflicts.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isIdentifierTaken distinguishes the two unique indexes a create can trip: the
// identifier of another DB instance, and the name of a live instance the
// backing row shares idx_instances_name_live with.
func isIdentifierTaken(err error) bool {
	return isUniqueViolation(err) && strings.Contains(err.Error(), "db_instances.identifier")
}
