// Package system answers questions about the control plane itself: which
// build is running, whether it still needs first-run setup, whether the
// hypervisor and the object store are reachable and how much of the host is
// spoken for.
package system

import (
	"context"
	"errors"
	"log/slog"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
)

// UserCounter reports how many user accounts exist. Zero means the instance
// has never been set up.
type UserCounter interface {
	CountUsers(ctx context.Context) (int64, error)
}

// HypervisorChecker reports the hypervisor backend and whether it answers.
type HypervisorChecker interface {
	Ping(ctx context.Context) error
}

// KVMReporter is the optional half of a backend that knows whether hardware
// acceleration is usable. libvirt implements it; the fake does not, so the
// field is null there instead of guessing. An error means the backend could not
// be asked, which also leaves the field null: the hypervisor's ok already says
// the daemon is down, and false would claim something nobody measured.
type KVMReporter interface {
	HasKVM(ctx context.Context) (bool, error)
}

// CapacityReporter is the optional half of a backend that can measure the host.
// Both shipped backends implement it; it is a separate seam from Ping because
// the fake answers Info long before it is asked about capacity.
type CapacityReporter interface {
	Capacity(ctx context.Context) (hypervisor.Capacity, error)
}

// Allocator reports the capacity held by the instances that exist.
type Allocator interface {
	// AllocatedCapacity returns the vCPUs, memory and root disk of every
	// instance that is not terminated.
	AllocatedCapacity(ctx context.Context) (hypervisor.Capacity, error)
}

// Info is the payload of GET /api/v1/system/info.
type Info struct {
	Version    string         `json:"version"`
	NeedsSetup bool           `json:"needs_setup"`
	Hypervisor HypervisorInfo `json:"hypervisor"`
	// ObjectStorage tells the console whether to offer the Storage pages.
	ObjectStorage ObjectStorageInfo `json:"object_storage"`
	// Databases tells the console whether to offer the Databases pages, and
	// what a DB instance can be created from.
	Databases DatabasesInfo `json:"databases"`
	// Kubernetes tells the console whether to offer the Kubernetes pages, and
	// which versions a cluster can be created at.
	Kubernetes KubernetesInfo `json:"kubernetes"`
}

// KubernetesInfo describes the Kubernetes module
// (docs/design/08-kubernetes.md section 2). Versions is the Kubernetes minors
// with an available engine image, newest first, and stays empty until one is
// imported -- which, as for databases, is a state the console explains rather
// than a fault.
type KubernetesInfo struct {
	Enabled  bool     `json:"enabled"`
	Versions []string `json:"versions"`
}

// VersionLister reports the Kubernetes versions a cluster can be created at.
type VersionLister func(ctx context.Context) ([]string, error)

// DatabasesInfo describes the databases module (docs/design/07-databases.md
// section 2). Enabled is the configuration; Engines is what a DB instance can
// actually be created from, which stays empty until an engine image is
// imported.
//
// Enabled with an empty Engines list is not an error and not a
// misconfiguration: it is a node whose operator has not imported a golden
// image yet, and the console explains the import rather than reporting a
// fault.
//
// The image id is deliberately absent. This payload is served without a token,
// the same reason the object store's endpoint is left out, and nothing needs
// it: a create request names an engine and a version, never an image.
type DatabasesInfo struct {
	Enabled bool                 `json:"enabled"`
	Engines []DatabaseEngineInfo `json:"engines"`
}

// DatabaseEngineInfo is one engine and version a DB instance can be created
// from.
type DatabaseEngineInfo struct {
	Engine  string `json:"engine"`
	Version string `json:"version"`
}

// EngineLister reports the engines a DB instance can be created from. It is a
// function rather than an interface so that this package does not depend on
// the image service for one struct; the wiring adapts.
type EngineLister func(ctx context.Context) ([]DatabaseEngineInfo, error)

// ObjectStorageInfo describes the object storage module. Backend "none" means
// the module is off; OK is whether the server answered Kumo's credentials.
// The endpoint is left out on purpose: this payload is served without a
// token, and the address belongs to the signed-in pages.
type ObjectStorageInfo struct {
	Backend string `json:"backend"`
	OK      bool   `json:"ok"`
}

// HypervisorInfo describes the hypervisor backend. KVM is nil when the backend
// cannot answer, which is the case for the in-memory fake.
type HypervisorInfo struct {
	Type string `json:"type"`
	OK   bool   `json:"ok"`
	KVM  *bool  `json:"kvm"`
}

// Service implements Info and Capacity.
type Service struct {
	logger        *slog.Logger
	version       string
	hypervisor    string
	users         UserCounter
	hypervisorPin HypervisorChecker
	allocator     Allocator
	limits        capacity.Limits
	objectBackend string
	objectPin     HypervisorChecker
	databases     bool
	engines       EngineLister
	kubernetes    bool
	k8sVersions   VersionLister
}

// New builds the service. users, hypervisorPin and allocator may be nil, which
// happens only in the API tests built without a database or a hypervisor; the
// service then reports needs_setup=false, a hypervisor that is not ok and
// nothing allocated.
func New(
	logger *slog.Logger,
	version, hypervisorName string,
	users UserCounter,
	hypervisorPin HypervisorChecker,
	allocator Allocator,
) *Service {
	return &Service{
		logger:        logger,
		version:       version,
		hypervisor:    hypervisorName,
		users:         users,
		hypervisorPin: hypervisorPin,
		allocator:     allocator,
		limits:        capacity.DefaultLimits(),
		objectBackend: "none",
	}
}

// SetObjectStore reports the object storage module in Info. pin is nil when
// the module is off.
func (s *Service) SetObjectStore(backend string, pin HypervisorChecker) {
	s.objectBackend = backend
	s.objectPin = pin
}

// SetKubernetes reports the Kubernetes module in Info.
func (s *Service) SetKubernetes(enabled bool, versions VersionLister) {
	s.kubernetes = enabled
	s.k8sVersions = versions
}

// SetDatabases reports the databases module in Info. engines is nil when the
// module is off, or in the API tests built without an image service, and Info
// then reports no engines rather than failing.
func (s *Service) SetDatabases(enabled bool, engines EngineLister) {
	s.databases = enabled
	s.engines = engines
}

// SetLimits applies the configured reserve and overcommit ratio. The launch check
// uses the same limits, which is what keeps GET /system/capacity and the refusal
// a launch can get from telling the same story.
func (s *Service) SetLimits(limits capacity.Limits) { s.limits = limits }

// Info collects the current control plane status.
func (s *Service) Info(ctx context.Context) (Info, error) {
	info := Info{
		Version:       s.version,
		NeedsSetup:    false,
		Hypervisor:    HypervisorInfo{Type: s.hypervisor, OK: false},
		ObjectStorage: ObjectStorageInfo{Backend: s.objectBackend, OK: false},
		Databases: DatabasesInfo{
			Enabled: s.databases,
			// Never nil: the console reads this as a list, and a JSON null
			// would make "no engine image yet" an error case for it.
			Engines: []DatabaseEngineInfo{},
		},
		Kubernetes: KubernetesInfo{Enabled: s.kubernetes, Versions: []string{}},
	}
	if s.kubernetes && s.k8sVersions != nil {
		if versions, err := s.k8sVersions(ctx); err != nil {
			s.logger.WarnContext(ctx, "cannot list the Kubernetes versions", "error", err)
		} else if len(versions) > 0 {
			info.Kubernetes.Versions = versions
		}
	}

	if s.users != nil {
		count, err := s.users.CountUsers(ctx)
		if err != nil {
			return Info{}, err
		}
		info.NeedsSetup = count == 0
	}

	if s.hypervisorPin != nil {
		if err := s.hypervisorPin.Ping(ctx); err != nil {
			s.logger.WarnContext(ctx, "hypervisor is not reachable",
				"hypervisor", s.hypervisor, "error", err)
		} else {
			info.Hypervisor.OK = true
		}
	}

	if s.objectPin != nil {
		if err := s.objectPin.Ping(ctx); err != nil {
			s.logger.WarnContext(ctx, "the object store is not reachable",
				"backend", s.objectBackend, "error", err)
		} else {
			info.ObjectStorage.OK = true
		}
	}

	// The engines are read only while the module is on: with it off the routes
	// answer 503 anyway, and a query per /system/info call would be work for
	// an answer nobody acts on.
	if s.databases && s.engines != nil {
		engines, err := s.engines(ctx)
		if err != nil {
			// An unreadable image table is not a reason to fail the whole
			// status payload, which the console polls to decide what to
			// render. It reports no engines and says so in the log.
			s.logger.WarnContext(ctx, "cannot list the database engines", "error", err)
		} else if len(engines) > 0 {
			info.Databases.Engines = engines
		}
	}

	// A nil KVM means the backend has no opinion (the fake); a pointer to false
	// is the important signal: the daemon is up but no guest can boot.
	if reporter, ok := s.hypervisorPin.(KVMReporter); ok {
		kvm, err := reporter.HasKVM(ctx)
		if err != nil {
			s.logger.DebugContext(ctx, "cannot ask the hypervisor about KVM", "error", err)
		} else {
			info.Hypervisor.KVM = &kvm
		}
	}

	return info, nil
}

// Capacity reports the host totals against what the instances hold.
//
// The totals come from the backend and are all a hypervisor can know; the
// allocated half is summed from the instances that are not terminated. A backend
// that cannot measure itself reports zero totals rather than failing the request,
// so the console still renders the allocated side and says the host did not
// answer.
func (s *Service) Capacity(ctx context.Context) (capacity.Report, error) {
	var total hypervisor.Capacity

	if reporter, ok := s.hypervisorPin.(CapacityReporter); ok {
		measured, err := reporter.Capacity(ctx)
		switch {
		case err == nil:
			total = measured
		case errors.Is(err, hypervisor.ErrNotImplemented):
			s.logger.WarnContext(ctx, "the hypervisor cannot report its capacity")
		default:
			return capacity.Report{}, err
		}
	}

	var allocated hypervisor.Capacity
	if s.allocator != nil {
		held, err := s.allocator.AllocatedCapacity(ctx)
		if err != nil {
			return capacity.Report{}, err
		}
		allocated = held
	}

	return capacity.Compute(total, allocated, s.limits), nil
}
