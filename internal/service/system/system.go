// Package system answers questions about the control plane itself: which
// build is running, whether it still needs first-run setup, whether the
// hypervisor is reachable and how much of the host is spoken for.
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
// field is null there instead of guessing.
type KVMReporter interface {
	HasKVM() bool
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
	}
}

// SetLimits applies the configured reserve and overcommit ratio. The launch check
// uses the same limits, which is what keeps GET /system/capacity and the refusal
// a launch can get from telling the same story.
func (s *Service) SetLimits(limits capacity.Limits) { s.limits = limits }

// Info collects the current control plane status.
func (s *Service) Info(ctx context.Context) (Info, error) {
	info := Info{
		Version:    s.version,
		NeedsSetup: false,
		Hypervisor: HypervisorInfo{Type: s.hypervisor, OK: false},
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

	// A nil KVM means the backend has no opinion (the fake); a pointer to false
	// is the important signal: the daemon is up but no guest can boot.
	if reporter, ok := s.hypervisorPin.(KVMReporter); ok {
		kvm := reporter.HasKVM()
		info.Hypervisor.KVM = &kvm
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
