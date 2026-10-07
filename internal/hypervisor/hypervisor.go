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
	"strings"
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
	// ErrInvalidSpec is returned when a caller passes a spec that could only
	// produce something the backend would reject, for example a domain with no
	// root disk.
	ErrInvalidSpec = errors.New("hypervisor: invalid spec")
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
	//
	// ImportImage registers an image as a volume in the images pool. The spec
	// is passed in rather than sniffed, because the caller has already parsed
	// the header and libvirt needs the capacity up front for the upload path
	// (docs/design/04-libvirt-integration.md §2).
	ImportImage(ctx context.Context, name string, r io.Reader, spec ImportSpec) error
	ImageInfo(ctx context.Context, name string) (ImageInfo, error)
	DeleteVolume(ctx context.Context, pool, name string) error
	CreateRootDisk(ctx context.Context, spec RootDiskSpec) error
	// CreateSeedISO writes the rendered seed data as a volume in pool. The pool
	// is named rather than assumed because a seed ISO is per-instance data and
	// belongs beside the root disks, not with the images every instance shares.
	CreateSeedISO(ctx context.Context, pool, name string, seed CloudInit) error

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

	// Networks. One VPC is one libvirt network (docs/design/04-libvirt-integration.md §6).
	// DefineNetwork is a definition only; StartNetwork activates it, the way a
	// VPC create task defines then starts.
	DefineNetwork(ctx context.Context, spec NetworkSpec) error
	StartNetwork(ctx context.Context, name string) error
	DestroyNetwork(ctx context.Context, name string) error
	UndefineNetwork(ctx context.Context, name string) error
	GetNetwork(ctx context.Context, name string) (NetworkInfo, error)
	// AddDHCPHost installs a static <host mac ip name> entry via virNetworkUpdate;
	// DeleteDHCPHost removes it again, matching on the MAC.
	AddDHCPHost(ctx context.Context, network string, host DHCPHost) error
	DeleteDHCPHost(ctx context.Context, network string, mac string) error

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

// ImportSpec describes an image file being registered as a volume.
//
// SizeBytes and VirtualSizeBytes are separate on purpose: a 500 MiB qcow2 with a
// 10 GiB virtual size is the normal case, and conflating them either truncates
// the image or hands libvirt a capacity too small to boot.
type ImportSpec struct {
	// SizeBytes is how many bytes r yields, which bounds a copy or an upload.
	SizeBytes int64
	// VirtualSizeBytes is the capacity the guest sees. It is what libvirt puts
	// in <capacity>.
	VirtualSizeBytes int64
	// Format is the on-disk format the header parse found.
	Format ImageFormat
}

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
//
// Every document arrives rendered. The rules about which key goes into which
// user-data shape live in internal/service/cloudinit, so a backend never has to
// know what a cloud-config is, and the two backends cannot drift apart on what a
// seed looks like.
type CloudInit struct {
	// MetaData is the NoCloud meta-data document, which carries the instance id
	// and the hostname.
	MetaData string
	// UserData is the cloud-init payload, empty when the launch supplied none.
	UserData string
	// NetworkConfig is the network-config v2 document, empty when the seed
	// should leave the guest's networking to the image.
	NetworkConfig string
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
	// MACAddress is the MAC the guest's virtio NIC is created with, from the
	// interface the launch allocated. Empty when a launch predates interfaces:
	// the backend then lets libvirt pick a MAC itself.
	MACAddress string
}

// NetworkMode is the forwarding mode of a VPC's libvirt network: nat reaches
// the LAN and the Internet through the host, isolated keeps guests on the
// bridge talking only to each other and the gateway.
type NetworkMode string

const (
	NetworkModeNAT      NetworkMode = "nat"
	NetworkModeIsolated NetworkMode = "isolated"
)

// NetworkSpec is everything needed to define a VPC's libvirt network. The
// service derives every field from the VPC row (docs/design/04 §6), so a
// backend just writes what it is told.
type NetworkSpec struct {
	// Name is the libvirt network name, the VPC id itself (the way a domain is
	// named after an instance id). "kumo-default" is the adopted default.
	Name string
	// Bridge is the bridge interface name the network lives on, kmb<8 hex> of
	// the VPC id unless the default network was adopted with its own bridge.
	Bridge string
	// CIDR is the VPC's address block.
	CIDR string
	// Gateway is the address libvirt puts on the bridge and dnsmasq serves
	// from: the first host address of the VPC CIDR.
	Gateway string
	// Mode is nat or isolated.
	Mode NetworkMode
	// Domain seeds the libvirt DNS domain, from the VPC name.
	Domain string
}

// DHCPHost is one static lease of a network: dnsmasq serves exactly what Kumo
// allocates, and nothing else (docs/design/04 §6.4).
type DHCPHost struct {
	MAC  string
	IP   string
	Name string
}

// NetworkInfo is the state of a libvirt network, read back for adoption and for
// the delete path.
type NetworkInfo struct {
	Name      string
	Bridge    string
	CIDR      string
	Gateway   string
	Mode      NetworkMode
	Domain    string
	Active    bool
	DHCPHosts []DHCPHost
}

// RootDiskName is the volume name of an instance's root disk. The service that
// launches, the libvirt backend that creates it and the fake that holds it all
// derive the name here, so a rollback cannot delete a volume under a different
// name than the one the launch created (docs/design/04-libvirt-integration.md §2).
func RootDiskName(instanceID string) string { return instanceID + "-root.qcow2" }

// SeedISOName is the volume name of an instance's NoCloud seed, for the same
// reason as RootDiskName.
func SeedISOName(instanceID string) string { return instanceID + "-seed.iso" }

// BridgeName derives the bridge interface of a VPC's libvirt network: "kmb"
// plus the first 8 hex characters of the VPC id, e.g. kmb0a1b2c3d. That length
// (11 characters) fits Linux's IFNAMSIZ limit of 15, and the VPC id's
// randomness keeps bridges unique (docs/design/04-libvirt-integration.md §6.1).
func BridgeName(vpcID string) string {
	hexPart := strings.TrimPrefix(vpcID, "vpc-")
	if len(hexPart) > 8 {
		hexPart = hexPart[:8]
	}
	return "kmb" + hexPart
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
	OpPing            Op = "ping"
	OpEnsureInfra     Op = "ensure_infra"
	OpImportImage     Op = "import_image"
	OpImageInfo       Op = "image_info"
	OpDeleteVolume    Op = "delete_volume"
	OpCreateRootDisk  Op = "create_root_disk"
	OpCreateSeedISO   Op = "create_seed_iso"
	OpDefineAndStart  Op = "define_and_start"
	OpStart           Op = "start"
	OpShutdown        Op = "shutdown"
	OpDestroy         Op = "destroy"
	OpReboot          Op = "reboot"
	OpReset           Op = "reset"
	OpUndefine        Op = "undefine"
	OpGetDomain       Op = "get_domain"
	OpListDomains     Op = "list_domains"
	OpConsoleOutput   Op = "console_output"
	OpCapacity        Op = "capacity"
	OpEvents          Op = "events"
	OpDefineNetwork   Op = "define_network"
	OpStartNetwork    Op = "start_network"
	OpDestroyNetwork  Op = "destroy_network"
	OpUndefineNetwork Op = "undefine_network"
	OpGetNetwork      Op = "get_network"
	OpAddDHCPHost     Op = "add_dhcp_host"
	OpDeleteDHCPHost  Op = "delete_dhcp_host"
)
