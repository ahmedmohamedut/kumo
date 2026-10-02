// Package hypervisor is the boundary between the Kumo services and the
// virtualization backend. Services depend on the Hypervisor interface only;
// the libvirt and in-memory fake implementations live in sub-packages.
//
// See docs/design/04-libvirt-integration.md.
package hypervisor

import (
	"context"
	"errors"
	"io"
)

// Sentinel errors. Implementations return them (possibly wrapped) so services
// can map them onto service errors and HTTP statuses in one place.
var (
	// ErrNotFound is returned for unknown domains, volumes and pools.
	ErrNotFound = errors.New("hypervisor: not found")
	// ErrAlreadyExists is returned when defining a domain or volume that is
	// already known to the backend.
	ErrAlreadyExists = errors.New("hypervisor: already exists")
	// ErrInvalidState is returned when a lifecycle transition does not apply to
	// the current state, for example starting a running domain.
	ErrInvalidState = errors.New("hypervisor: invalid state")
	// ErrNotImplemented is returned by backends whose features are not built
	// yet, so callers can degrade instead of crashing.
	ErrNotImplemented = errors.New("hypervisor: not implemented")
)

// Hypervisor is the set of operations Kumo needs from a virtualization
// backend. Implementations must be safe for concurrent use.
type Hypervisor interface {
	// Ping reports whether the backend is reachable.
	Ping(ctx context.Context) error
	// EnsureInfra creates the storage pools and network, idempotently.
	EnsureInfra(ctx context.Context, cfg InfraConfig) error

	// Volumes.
	ImportImage(ctx context.Context, name string, r io.Reader, size int64) error
	ImageInfo(ctx context.Context, name string) (ImageInfo, error)
	DeleteVolume(ctx context.Context, pool, name string) error
	CreateRootDisk(ctx context.Context, spec RootDiskSpec) error
	CreateSeedISO(ctx context.Context, name string, seed CloudInit) error

	// Domains.
	DefineAndStart(ctx context.Context, spec DomainSpec) (DomainInfo, error)
	Start(ctx context.Context, id string) error
	Shutdown(ctx context.Context, id string) error // ACPI request
	Destroy(ctx context.Context, id string) error  // hard stop
	Reboot(ctx context.Context, id string) error
	Reset(ctx context.Context, id string) error
	Undefine(ctx context.Context, id string) error
	GetDomain(ctx context.Context, id string) (DomainInfo, error)
	ListDomains(ctx context.Context) ([]DomainInfo, error)
	ConsoleOutput(ctx context.Context, id string, maxBytes int) ([]byte, error)
	Capacity(ctx context.Context) (Capacity, error)

	// Events streams lifecycle changes until ctx is canceled.
	Events(ctx context.Context) (<-chan DomainEvent, error)
}

// InfraConfig names the pools and network EnsureInfra must create.
type InfraConfig struct {
	ImagesPool    string
	InstancesPool string
	Network       string
	DataDir       string
}

// ImageFormat is the on-disk format of an imported image.
type ImageFormat string

const (
	// FormatQCOW2 is a qcow2 copy-on-write image.
	FormatQCOW2 ImageFormat = "qcow2"
	// FormatRaw is a plain sector image.
	FormatRaw ImageFormat = "raw"
)

// ImageInfo describes a volume in the images pool.
type ImageInfo struct {
	Name             string
	Pool             string
	Format           ImageFormat
	SizeBytes        int64 // bytes stored on the host
	VirtualSizeBytes int64 // bytes the guest sees
}

// RootDiskSpec describes the per-instance root disk to create: a copy-on-write
// overlay on top of an image volume, sized to SizeGiB.
type RootDiskSpec struct {
	InstanceID   string
	SourceVolume string // volume name in the images pool
	Pool         string
	SizeGiB      int64
}

// CloudInit is the seed data rendered into an instance's NoCloud ISO.
type CloudInit struct {
	InstanceID string
	Hostname   string
	UserData   string
}

// DomainSpec is everything needed to write a domain definition.
type DomainSpec struct {
	ID             string
	Name           string
	UUID           string
	VCPUs          int
	MemoryMiB      int
	Machine        string
	Arch           string
	Firmware       string
	RootDiskPool   string
	RootDiskPath   string
	SeedISOPath    string
	ConsoleLogPath string
	Network        string
}

// DomainState is the backend state of a domain, mapped to instance states by
// the reconciler.
type DomainState string

const (
	DomainStateRunning DomainState = "running"
	DomainStatePaused  DomainState = "paused"
	DomainStateShutoff DomainState = "shutoff"
	DomainStateCrashed DomainState = "crashed"
)

// DomainInfo is the runtime state of a single domain.
type DomainInfo struct {
	ID        string
	Name      string
	UUID      string
	State     DomainState
	IP        string
	MAC       string
	VCPUs     int
	MemoryMiB int
}

// DomainEventAction is what happened to a domain.
type DomainEventAction string

const (
	ActionDefined  DomainEventAction = "defined"
	ActionStarted  DomainEventAction = "started"
	ActionShutdown DomainEventAction = "shutdown"
	ActionStopped  DomainEventAction = "stopped"
	ActionReboot   DomainEventAction = "reboot"
	ActionCrashed  DomainEventAction = "crashed"
	ActionUndefine DomainEventAction = "undefined"
)

// DomainEvent is a single lifecycle transition.
type DomainEvent struct {
	ID     string
	Action DomainEventAction
	State  DomainState
}

// Capacity is the host's total capacity. Allocated capacity is derived from the
// domains and instances that exist.
type Capacity struct {
	VCPUs       int
	MemoryBytes int64
	DiskBytes   int64
}

// Op names an operation of the Hypervisor interface. Implementations use it to
// inject failures in tests and in `KUMO_HYPERVISOR=fake`.
type Op string

const (
	OpPing           Op = "ping"
	OpEnsureInfra    Op = "ensure_infra"
	OpImportImage    Op = "import_image"
	OpImageInfo      Op = "image_info"
	OpDeleteVolume   Op = "delete_volume"
	OpCreateRootDisk Op = "create_root_disk"
	OpCreateSeedISO  Op = "create_seed_iso"
	OpDefineAndStart Op = "define_and_start"
	OpStart          Op = "start"
	OpShutdown       Op = "shutdown"
	OpDestroy        Op = "destroy"
	OpReboot         Op = "reboot"
	OpReset          Op = "reset"
	OpUndefine       Op = "undefine"
	OpGetDomain      Op = "get_domain"
	OpListDomains    Op = "list_domains"
	OpConsoleOutput  Op = "console_output"
	OpCapacity       Op = "capacity"
	OpEvents         Op = "events"
)
