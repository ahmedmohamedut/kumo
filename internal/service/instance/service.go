// Package instance owns instances: launching them, moving them through their
// lifecycle, listing them and reading one back.
//
// A launch is a long operation, so POST /instances only writes the row and a
// task and answers 202; the root disk, the seed ISO, the domain and the address
// are made by the worker running the instance.launch task
// (docs/design/04-libvirt-integration.md §3). Nothing in this package blocks an
// HTTP request on libvirt. The lifecycle actions of M4.4 follow the same rule:
// each one writes the state it implies and a task, and the worker runs it.
package instance

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
)

// State is the lifecycle of an instance. The values mirror the CHECK constraint
// on instances.state (docs/design/02-data-model.md) and the InstanceState enum of
// api/openapi.yaml.
//
// The row is the console's view; libvirt is the truth about the domain. A state
// here is what the control plane has recorded, which is why a launch lives in
// pending until the worker has seen the guest running.
type State string

const (
	// StatePending means the launch is queued or under way.
	StatePending State = "pending"
	// StateRunning means the domain is up and the row carries its address.
	StateRunning State = "running"
	// StateStopping means an ACPI shutdown is in flight.
	StateStopping State = "stopping"
	// StateStopped means the domain is off and can be started again.
	StateStopped State = "stopped"
	// StateTerminating means a terminate is in flight.
	StateTerminating State = "terminating"
	// StateTerminated means the volumes are gone. It is the only state that
	// releases an allocation (docs/design/02-data-model.md).
	StateTerminated State = "terminated"
	// StateError means the workflow gave up; StateReason says why.
	StateError State = "error"
)

// Interface is one virtual NIC of an instance, the row of the network_interfaces
// table in the shape api/openapi.yaml promises. An instance that has not been
// terminated carries exactly one, device 0; a terminated instance carries none,
// because the row is deleted with the domain.
type Interface struct {
	ID          string `json:"id"`
	DeviceIndex int    `json:"device_index"`
	MACAddress  string `json:"mac_address"`
	PrivateIP   string `json:"private_ip"`
	SubnetID    string `json:"subnet_id"`
	VPCID       string `json:"vpc_id"`
}

// Instance is the projection of an instance row, in the shape
// api/openapi.yaml promises. Nullable columns are pointers so the JSON carries
// null rather than an empty string.
type Instance struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	State        State   `json:"state"`
	StateReason  *string `json:"state_reason"`
	ImageID      string  `json:"image_id"`
	ImageName    string  `json:"image_name"`
	InstanceType string  `json:"instance_type"`
	VCPUs        int     `json:"vcpus"`
	MemoryMiB    int     `json:"memory_mib"`
	RootDiskGB   int     `json:"root_disk_gb"`
	KeyPairID    *string `json:"key_pair_id"`
	KeyPairName  *string `json:"key_pair_name"`
	PrivateIP    *string `json:"private_ip"`
	MACAddress   *string `json:"mac_address"`
	// VPCID and SubnetID are the network placement of the instance, read from its
	// live interface. They are null for a terminated instance, whose interface
	// row is gone.
	VPCID    *string `json:"vpc_id"`
	SubnetID *string `json:"subnet_id"`
	// NetworkInterfaces carries every live interface, empty for a terminated
	// instance.
	NetworkInterfaces []Interface `json:"network_interfaces"`
	// UserData is only filled in for an administrator, and never by List: it can
	// carry a secret a user pasted into it.
	UserData *string `json:"user_data"`
	// TaskID is the task of the most recent state change, so the console can
	// follow a launch without guessing which task it was.
	TaskID       *string           `json:"task_id"`
	LaunchedAt   time.Time         `json:"launched_at"`
	TerminatedAt *time.Time        `json:"terminated_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	Tags         map[string]string `json:"tags"`
}

// Page is one page of an instance listing.
type Page struct {
	Items         []Instance `json:"items"`
	NextPageToken *string    `json:"next_page_token"`
}

// SortOrder selects the order of an instance page.
type SortOrder string

// The supported sort orders.
const (
	SortLaunchedDesc SortOrder = "launched_at:desc"
	SortLaunchedAsc  SortOrder = "launched_at:asc"
)

// Pagination bounds, the same as every other list endpoint.
const (
	// MinPageSize is the smallest page a client may ask for.
	MinPageSize = 1
	// DefaultPageSize is what a client that does not ask gets.
	DefaultPageSize = 50
	// MaxPageSize keeps one response inside what the console can render.
	MaxPageSize = 200
)

// Bounds on the wait for a guest address, as
// docs/design/04-libvirt-integration.md §3 step 4 prescribes: an instance that
// never takes an address is a launch that failed, not one that is still coming.
// The same bounds cover a start, which boots a guest that has to ask for an
// address again.
const (
	defaultPollInterval = 2 * time.Second
	defaultPollTimeout  = 120 * time.Second
)

// defaultStopTimeout is the watchdog of docs/design/04-libvirt-integration.md §3
// "Stop / start / reboot": how long a graceful stop waits for the guest to power
// off before the task gives up. A guest that ignores the ACPI request does not
// become off by waiting longer, so the wait is bounded and the operator is left
// with a force.
const defaultStopTimeout = 90 * time.Second

// Launch bounds, from the validation of POST /instances in
// docs/design/01-api.md §Instances.
const (
	// MaxNameLength is the longest an RFC 1123 label may be.
	MaxNameLength = 63
	// MaxRootDiskGB is the largest root disk a launch may ask for.
	MaxRootDiskGB = 2048
	// MaxUserDataBytes is the largest cloud-init payload accepted.
	MaxUserDataBytes = 16 << 10
	// MaxCount is the largest count the contract allows.
	MaxCount = 10
)

// VolumeResolver reports where a volume lives on the host, which the domain XML
// needs because libvirt attaches a disk by path.
//
// It is a seam of this package rather than a method on hypervisor.Hypervisor:
// resolving a path is only meaningful for a backend with a local filesystem, and
// the fake answers it without one.
type VolumeResolver interface {
	VolumePath(ctx context.Context, pool, name string) (string, error)
}

// Options configures the service.
type Options struct {
	// Logger records every state change of an instance.
	Logger *slog.Logger
	// Store is the metadata database.
	Store *store.Store
	// Hypervisor creates the volumes and the domain.
	Hypervisor hypervisor.Hypervisor
	// Volumes resolves a volume to a host path for the domain XML.
	Volumes VolumeResolver
	// Tasks creates the launch task.
	Tasks *task.Service
	// Images supplies the volume to launch from and the seed's default user.
	Images *image.Service
	// Types supplies the instance type preset.
	Types *instancetype.Service
	// KeyPairs supplies the public key a key pair launch injects.
	KeyPairs *keypair.Service
	// Capacity measures the host against the instances that exist. It is the same
	// service GET /system/capacity answers with, so the launch refusal and the
	// capacity the console shows cannot disagree
	// (docs/design/00-architecture.md §5).
	Capacity *system.Service
	// InstancesPool is the libvirt pool that holds the root disks and seeds.
	InstancesPool string
	// ImagesPool is the pool the image volumes live in.
	ImagesPool string
	// ConsoleDir is where the serial console log of each instance is written.
	ConsoleDir string
	// Network is the libvirt network the domain is attached to.
	Network string
	// Locks serializes the operations of one instance (docs/design/00-architecture.md
	// §4). A nil value gets a set of its own, which is right for a process that
	// runs one instance service; a test that has to hold a lock passes its own.
	Locks *locks.Set
	// SubnetLocks serializes the allocation of one subnet. Launch allocates an
	// address inside the launch transaction while a subnet delete would remove
	// the row, so both must take the very same key (subnet.LockKey). The set is
	// shared with the subnet service; a nil value gets one of its own.
	SubnetLocks *locks.Set
	// Limits are the capacity rules of the node.
	Limits capacity.Limits
	// PollInterval and PollTimeout bound the wait for a guest address, as
	// docs/design/04-libvirt-integration.md §3 step 4 describes.
	PollInterval time.Duration
	PollTimeout  time.Duration
	// StopTimeout bounds the wait for a graceful stop to reach the guest. It comes
	// from KUMO_STOP_TIMEOUT; a zero value gets defaultStopTimeout.
	StopTimeout time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Service owns instances.
type Service struct {
	logger        *slog.Logger
	store         *store.Store
	hv            hypervisor.Hypervisor
	volumes       VolumeResolver
	tasks         *task.Service
	images        *image.Service
	types         *instancetype.Service
	keyPairs      *keypair.Service
	capacity      *system.Service
	instancesPool string
	imagesPool    string
	consoleDir    string
	network       string
	locks         *locks.Set
	subnetLocks   *locks.Set
	limits        capacity.Limits
	pollInterval  time.Duration
	pollTimeout   time.Duration
	stopTimeout   time.Duration
	now           func() time.Time
}

// Sentinel errors of the constructor, so a wiring mistake reads differently from
// a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("instance: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("instance: a store is required")
	// ErrNoHypervisor means Options.Hypervisor was nil.
	ErrNoHypervisor = errors.New("instance: a hypervisor is required")
	// ErrNoVolumes means Options.Volumes was nil.
	ErrNoVolumes = errors.New("instance: a volume resolver is required")
	// ErrNoTasks means Options.Tasks was nil.
	ErrNoTasks = errors.New("instance: a task service is required")
	// ErrNoImages means Options.Images was nil.
	ErrNoImages = errors.New("instance: an image reader is required")
	// ErrNoTypes means Options.Types was nil.
	ErrNoTypes = errors.New("instance: an instance type reader is required")
	// ErrNoKeyPairs means Options.KeyPairs was nil.
	ErrNoKeyPairs = errors.New("instance: a key pair reader is required")
	// ErrNoCapacity means Options.Capacity was nil.
	ErrNoCapacity = errors.New("instance: a capacity reporter is required")
	// ErrNoInstancesPool means Options.InstancesPool was empty.
	ErrNoInstancesPool = errors.New("instance: an instances pool name is required")
	// ErrNoImagesPool means Options.ImagesPool was empty.
	ErrNoImagesPool = errors.New("instance: an images pool name is required")
	// ErrNoConsoleDir means Options.ConsoleDir was empty.
	ErrNoConsoleDir = errors.New("instance: a console directory is required")
	// ErrNoNetwork means Options.Network was empty.
	ErrNoNetwork = errors.New("instance: a network name is required")
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
	case opts.Volumes == nil:
		return nil, ErrNoVolumes
	case opts.Tasks == nil:
		return nil, ErrNoTasks
	case opts.Images == nil:
		return nil, ErrNoImages
	case opts.Types == nil:
		return nil, ErrNoTypes
	case opts.KeyPairs == nil:
		return nil, ErrNoKeyPairs
	case opts.Capacity == nil:
		return nil, ErrNoCapacity
	case opts.InstancesPool == "":
		return nil, ErrNoInstancesPool
	case opts.ImagesPool == "":
		return nil, ErrNoImagesPool
	case opts.ConsoleDir == "":
		return nil, ErrNoConsoleDir
	case opts.Network == "":
		return nil, ErrNoNetwork
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Locks == nil {
		opts.Locks = locks.New()
	}
	if opts.SubnetLocks == nil {
		opts.SubnetLocks = locks.New()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	if opts.PollTimeout <= 0 {
		opts.PollTimeout = defaultPollTimeout
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = defaultStopTimeout
	}
	if opts.Limits.VCPUOvercommit < 1 {
		opts.Limits.VCPUOvercommit = 1
	}

	return &Service{
		logger:        opts.Logger,
		store:         opts.Store,
		hv:            opts.Hypervisor,
		volumes:       opts.Volumes,
		tasks:         opts.Tasks,
		images:        opts.Images,
		types:         opts.Types,
		keyPairs:      opts.KeyPairs,
		capacity:      opts.Capacity,
		instancesPool: opts.InstancesPool,
		imagesPool:    opts.ImagesPool,
		consoleDir:    opts.ConsoleDir,
		network:       opts.Network,
		locks:         opts.Locks,
		subnetLocks:   opts.SubnetLocks,
		limits:        opts.Limits,
		pollInterval:  opts.PollInterval,
		pollTimeout:   opts.PollTimeout,
		stopTimeout:   opts.StopTimeout,
		now:           opts.Now,
	}, nil
}

// LockKey is the key this service serializes the operations of one instance with
// (docs/design/00-architecture.md §4). It is exported because the lock belongs to
// the instance rather than to the service: anything else that acts on the same
// instance has to take the very same key, or the two operations interleave.
func LockKey(instanceID string) string { return "instance:" + instanceID }

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

func nullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

// isUniqueViolation reports a UNIQUE constraint failure. The match is exact
// because a bare "constraint failed" also covers CHECK, NOT NULL and foreign key
// violations, which are not conflicts.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// isDBLocked reports a SQLite "database is locked" failure: another writer held
// the commit long enough that the busy timeout ran out. The launch retries it,
// because it is a race with another launch, not a condition of the request.
func isDBLocked(err error) bool {
	return err != nil && strings.Contains(err.Error(), "database is locked")
}
