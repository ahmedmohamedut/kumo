package system_test

import (
	"context"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
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

			svc := system.New(logging.Discard(), "v0.0.1", tt.hypervisor, tt.users, tt.pin)

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

func TestInfoPropagatesStoreErrors(t *testing.T) {
	t.Parallel()

	svc := system.New(logging.Discard(), "v0.0.1", "fake",
		stubUsers{err: context.Canceled}, stubHypervisor{})

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
