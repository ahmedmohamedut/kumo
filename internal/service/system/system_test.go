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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc := system.New(logging.Discard(), "v0.0.1", tt.hypervisor, tt.users, tt.pin)

			got, err := svc.Info(context.Background())
			if err != nil {
				t.Fatalf("Info() error = %v", err)
			}
			if got != tt.want {
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
