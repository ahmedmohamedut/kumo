package capacity_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
)

func TestComputeAppliesTheOvercommitRatioToVCPUs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		total     hypervisor.Capacity
		allocated hypervisor.Capacity
		limits    capacity.Limits
		want      capacity.Report
	}{
		{
			name:      "an empty node reports the whole host",
			total:     hypervisor.Capacity{VCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 250 << 30},
			allocated: hypervisor.Capacity{},
			limits:    capacity.Limits{VCPUOvercommit: 4},
			want: capacity.Report{
				VCPUs:       capacity.Metric{Total: 32},
				MemoryBytes: capacity.Metric{Total: 16 << 30},
				DiskBytes:   capacity.Metric{Total: 250 << 30},
			},
		},
		{
			name:  "allocated instances are counted as taken",
			total: hypervisor.Capacity{VCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 250 << 30},
			allocated: hypervisor.Capacity{
				VCPUs:       3,
				MemoryBytes: 6 << 30,
				DiskBytes:   60 << 30,
			},
			limits: capacity.Limits{VCPUOvercommit: 4},
			want: capacity.Report{
				VCPUs:       capacity.Metric{Total: 32, Allocated: 3},
				MemoryBytes: capacity.Metric{Total: 16 << 30, Allocated: 6 << 30},
				DiskBytes:   capacity.Metric{Total: 250 << 30, Allocated: 60 << 30},
			},
		},
		{
			name:      "a ratio below one is treated as one",
			total:     hypervisor.Capacity{VCPUs: 4},
			allocated: hypervisor.Capacity{},
			limits:    capacity.Limits{VCPUOvercommit: 0},
			want:      capacity.Report{VCPUs: capacity.Metric{Total: 4}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := capacity.Compute(tt.total, tt.allocated, tt.limits)
			if got != tt.want {
				t.Errorf("Compute() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFreeSubtractsTheReserveFromMemoryOnly(t *testing.T) {
	t.Parallel()

	total := hypervisor.Capacity{VCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 250 << 30}
	allocated := hypervisor.Capacity{VCPUs: 4, MemoryBytes: 4 << 30, DiskBytes: 40 << 30}
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}

	got := capacity.Free(capacity.Compute(total, allocated, limits), limits)
	want := hypervisor.Capacity{
		// 8 host vCPUs at 4x, less the 4 already taken.
		VCPUs:       28,
		MemoryBytes: (16-4)*capacity.BytesPerGiB - 1024*capacity.BytesPerMiB,
		DiskBytes:   (250 - 40) * capacity.BytesPerGiB,
	}
	if got != want {
		t.Errorf("Free() = %+v, want %+v", got, want)
	}
}

func TestFreeClampsAtZero(t *testing.T) {
	t.Parallel()

	// A node that is already oversubscribed must report nothing free rather
	// than a negative number the console would have to special-case.
	total := hypervisor.Capacity{VCPUs: 1, MemoryBytes: 1 << 30, DiskBytes: 1 << 30}
	allocated := hypervisor.Capacity{VCPUs: 10, MemoryBytes: 8 << 30, DiskBytes: 8 << 30}
	limits := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 1}

	got := capacity.Free(capacity.Compute(total, allocated, limits), limits)
	want := hypervisor.Capacity{}
	if got != want {
		t.Errorf("Free() = %+v, want %+v", got, want)
	}
}

func TestCheckRefusesWhatTheNodeCannotHold(t *testing.T) {
	t.Parallel()

	free := hypervisor.Capacity{VCPUs: 8, MemoryBytes: 4 << 30, DiskBytes: 100 << 30}

	tests := []struct {
		name       string
		free       hypervisor.Capacity
		want       capacity.Request
		wantErr    bool
		wantReason string
	}{
		{
			name: "a small instance fits",
			free: free,
			want: capacity.Request{VCPUs: 1, MemoryBytes: 2 << 30, DiskBytes: 20 << 30},
		},
		{
			name:       "too many vCPUs",
			free:       free,
			want:       capacity.Request{VCPUs: 16, MemoryBytes: 1 << 30, DiskBytes: 20 << 30},
			wantErr:    true,
			wantReason: "vCPUs requested",
		},
		{
			name:       "too much memory",
			free:       free,
			want:       capacity.Request{VCPUs: 1, MemoryBytes: 8 << 30, DiskBytes: 20 << 30},
			wantErr:    true,
			wantReason: "memory requested",
		},
		{
			name:       "more disk than the pool has left",
			free:       free,
			want:       capacity.Request{VCPUs: 1, MemoryBytes: 1 << 30, DiskBytes: 400 << 30},
			wantErr:    true,
			wantReason: "root disk",
		},
		{
			name: "exactly what is left is allowed",
			free: free,
			want: capacity.Request{VCPUs: 8, MemoryBytes: 4 << 30, DiskBytes: 100 << 30},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := capacity.Check(tt.free, tt.want, capacity.Limits{
				MemoryReserveMiB: 1024,
				VCPUOvercommit:   4,
			})
			if tt.wantErr {
				if err == nil {
					t.Fatal("Check() error = nil, want a quota error")
				}
				if !errors.Is(err, service.ErrQuota) {
					t.Errorf("Check() error = %v, want service.ErrQuota", err)
				}
				if !strings.Contains(err.Error(), tt.wantReason) {
					t.Errorf("Check() error = %q, want it to mention %q", err, tt.wantReason)
				}
				return
			}
			if err != nil {
				t.Errorf("Check() error = %v, want nil", err)
			}
		})
	}
}

func TestBuildConvertsTheUnitsTheAPISpeaks(t *testing.T) {
	t.Parallel()

	got := capacity.Build(2, 4096, 20)
	want := capacity.Request{VCPUs: 2, MemoryBytes: 4 << 30, DiskBytes: 20 << 30}
	if got != want {
		t.Errorf("Build() = %+v, want %+v", got, want)
	}
}

func TestDefaultLimitsAreTheDocumentedOnes(t *testing.T) {
	t.Parallel()

	got := capacity.DefaultLimits()
	want := capacity.Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
	if got != want {
		t.Errorf("DefaultLimits() = %+v, want %+v", got, want)
	}
}
