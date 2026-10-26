package system_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

// The two failures the capacity report can hit. Static, so a test can assert
// that the service passed the backend's own error on instead of inventing its
// own on the way out.
var (
	errNodeUnavailable = errors.New("libvirt is restarting")
	errDatabaseLocked  = errors.New("database is locked")
)

type stubUsers struct {
	count int64
	err   error
}

func (s stubUsers) CountUsers(context.Context) (int64, error) { return s.count, s.err }

type stubHypervisor struct {
	err error
}

func (s stubHypervisor) Ping(context.Context) error { return s.err }

// stubKVM is a libvirt-shaped backend: it answers Ping and also knows whether
// /dev/kvm is usable, which the fake cannot report.
type stubKVM struct {
	stubHypervisor
	kvm bool
}

func (s stubKVM) HasKVM() bool { return s.kvm }

func TestInfo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		hypervisor string
		users      system.UserCounter
		pin        system.HypervisorChecker
		want       system.Info
	}{
		{
			name:       "no users means the instance needs setup",
			hypervisor: "fake",
			users:      stubUsers{count: 0},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: true,
				Hypervisor: system.HypervisorInfo{Type: "fake", OK: false},
			},
		},
		{
			name:       "users exist",
			hypervisor: "fake",
			users:      stubUsers{count: 3},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "fake", OK: false},
			},
		},
		{
			name:       "reachable hypervisor",
			hypervisor: "fake",
			users:      stubUsers{count: 1},
			pin:        stubHypervisor{},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "fake", OK: true},
			},
		},
		{
			name:       "unreachable hypervisor is reported, not fatal",
			hypervisor: "libvirt",
			users:      stubUsers{count: 1},
			pin:        stubHypervisor{err: context.DeadlineExceeded},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "libvirt", OK: false},
			},
		},
		{
			name:       "no store or hypervisor wired yet",
			hypervisor: "fake",
			users:      nil,
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "fake", OK: false},
			},
		},
		{
			// The daemon answers but hardware acceleration is missing: the
			// control plane is up, but no guest can boot.
			name:       "libvirt without kvm",
			hypervisor: "libvirt",
			pin:        stubKVM{stubHypervisor: stubHypervisor{}, kvm: false},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "libvirt", OK: true, KVM: boolPtr(false)},
			},
		},
		{
			name:       "libvirt with kvm",
			hypervisor: "libvirt",
			pin:        stubKVM{stubHypervisor: stubHypervisor{}, kvm: true},
			want: system.Info{
				Version:    "v0.0.1",
				NeedsSetup: false,
				Hypervisor: system.HypervisorInfo{Type: "libvirt", OK: true, KVM: boolPtr(true)},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := system.New(logging.Discard(), "v0.0.1", tt.hypervisor, tt.users, tt.pin, nil)

			got, err := svc.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v", err)
			}
			// The KVM field is a pointer so the fake can say "no opinion";
			// compare the values behind it rather than the addresses.
			if got.Version != tt.want.Version ||
				got.NeedsSetup != tt.want.NeedsSetup ||
				got.Hypervisor.Type != tt.want.Hypervisor.Type ||
				got.Hypervisor.OK != tt.want.Hypervisor.OK ||
				!sameBool(got.Hypervisor.KVM, tt.want.Hypervisor.KVM) {
				t.Errorf("Info() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

var errRefused = errors.New("connection refused")

func TestInfoReportsObjectStorage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		backend string
		pin     system.HypervisorChecker
		want    system.ObjectStorageInfo
	}{
		{name: "off", backend: "none", want: system.ObjectStorageInfo{Backend: "none"}},
		{name: "reachable", backend: "rustfs", pin: stubHypervisor{}, want: system.ObjectStorageInfo{Backend: "rustfs", OK: true}},
		{
			name: "unreachable", backend: "rustfs", pin: stubHypervisor{err: errRefused},
			want: system.ObjectStorageInfo{Backend: "rustfs", OK: false},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			svc := system.New(logging.Discard(), "v0.0.1", "fake", nil, stubHypervisor{}, nil)
			if tt.backend != "none" {
				svc.SetObjectStore(tt.backend, tt.pin)
			}
			got, err := svc.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v", err)
			}
			if got.ObjectStorage != tt.want {
				t.Errorf("ObjectStorage = %+v, want %+v", got.ObjectStorage, tt.want)
			}
			if !got.Hypervisor.OK {
				t.Error("an object store problem changed the hypervisor status")
			}
		})
	}
}

func TestInfoPropagatesStoreErrors(t *testing.T) {
	t.Parallel()

	svc := system.New(logging.Discard(), "v0.0.1", "fake",
		stubUsers{err: context.Canceled}, stubHypervisor{}, nil)

	if _, err := svc.Info(context.Background()); err == nil {
		t.Fatal("Info() error = nil, want the store error")
	}
}

// boolPtr is the address of a bool, for the KVM field.
func boolPtr(v bool) *bool { return &v }

// sameBool compares two optional bools by value; two nils are equal.
func sameBool(a, b *bool) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// stubCapacity is a backend that can also measure the host, which is what both
// shipped backends do.
type stubCapacity struct {
	stubHypervisor
	total hypervisor.Capacity
	err   error
}

func (s stubCapacity) Capacity(context.Context) (hypervisor.Capacity, error) {
	return s.total, s.err
}

type stubAllocator struct {
	allocated hypervisor.Capacity
	err       error
}

func (s stubAllocator) AllocatedCapacity(context.Context) (hypervisor.Capacity, error) {
	return s.allocated, s.err
}

func TestCapacity(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pin       system.HypervisorChecker
		allocator system.Allocator
		want      capacity.Report
	}{
		{
			name:      "totals from the host against the instances that exist",
			pin:       stubCapacity{total: hypervisor.Capacity{VCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 250 << 30}},
			allocator: stubAllocator{allocated: hypervisor.Capacity{VCPUs: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30}},
			want: capacity.Report{
				// 8 vCPUs at the default ratio of 4.
				VCPUs:       capacity.Metric{Total: 32, Allocated: 2},
				MemoryBytes: capacity.Metric{Total: 16 << 30, Allocated: 4 << 30},
				DiskBytes:   capacity.Metric{Total: 250 << 30, Allocated: 20 << 30},
			},
		},
		{
			name:      "a backend that cannot measure itself reports zeros, not an error",
			pin:       stubCapacity{err: hypervisor.ErrNotImplemented},
			allocator: stubAllocator{allocated: hypervisor.Capacity{VCPUs: 1, MemoryBytes: 2 << 30, DiskBytes: 20 << 30}},
			want: capacity.Report{
				VCPUs:       capacity.Metric{Allocated: 1},
				MemoryBytes: capacity.Metric{Allocated: 2 << 30},
				DiskBytes:   capacity.Metric{Allocated: 20 << 30},
			},
		},
		{
			name:      "a backend that only answers Ping contributes nothing",
			pin:       stubHypervisor{},
			allocator: stubAllocator{},
			want:      capacity.Report{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := system.New(logging.Discard(), "v0.0.1", "fake", nil, tt.pin, tt.allocator)

			got, err := svc.Capacity(context.Background())
			if err != nil {
				t.Fatalf("Capacity() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Capacity() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCapacityPropagatesRealFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		pin       system.HypervisorChecker
		allocator system.Allocator
		wantErr   error
	}{
		{
			name:      "the host could not be measured",
			pin:       stubCapacity{err: errNodeUnavailable},
			allocator: stubAllocator{},
			wantErr:   errNodeUnavailable,
		},
		{
			name:      "the database could not be summed",
			pin:       stubCapacity{total: hypervisor.Capacity{VCPUs: 1}},
			allocator: stubAllocator{err: errDatabaseLocked},
			wantErr:   errDatabaseLocked,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := system.New(logging.Discard(), "v0.0.1", "fake", nil, tt.pin, tt.allocator)

			_, err := svc.Capacity(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Capacity() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestCapacityUsesTheConfiguredLimits(t *testing.T) {
	t.Parallel()

	svc := system.New(logging.Discard(), "v0.0.1", "fake", nil,
		stubCapacity{total: hypervisor.Capacity{VCPUs: 8, MemoryBytes: 16 << 30}},
		stubAllocator{})
	svc.SetLimits(capacity.Limits{VCPUOvercommit: 2})

	got, err := svc.Capacity(context.Background())
	if err != nil {
		t.Fatalf("Capacity() error = %v", err)
	}
	if got.VCPUs.Total != 16 {
		t.Errorf("Capacity().VCPUs.Total = %d, want 16 at the configured ratio of 2", got.VCPUs.Total)
	}
}
