// Package capacity decides how much of a node an instance may claim, and
// reports what is left. The rules are the KUMO_MEMORY_RESERVE_MIB and
// KUMO_VCPU_OVERCOMMIT variables of docs/design/00-architecture.md §5, kept in
// one place because two callers need them: the launch request refuses a guest
// the node cannot hold, and GET /system/capacity reports the same arithmetic so
// the console and the refusal can never disagree.
package capacity

import (
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/service"
)

// Limits are the capacity rules of a node.
type Limits struct {
	// MemoryReserveMiB is held back from the host's memory. Memory is not
	// overcommitted, so this reserve is all that stands between a launch and a
	// node that starts swapping.
	MemoryReserveMiB int64
	// VCPUOvercommit is how many guest vCPUs one host vCPU may carry. A vCPU
	// that is idle costs nothing, so the ratio is what keeps a node from
	// accepting twenty-four single-vCPU guests on four cores.
	VCPUOvercommit int
}

// DefaultLimits are KUMO_MEMORY_RESERVE_MIB=1024 and KUMO_VCPU_OVERCOMMIT=4.
func DefaultLimits() Limits {
	return Limits{MemoryReserveMiB: 1024, VCPUOvercommit: 4}
}

// Metric is one resource, split into what the node has and what is taken.
type Metric struct {
	Total     int64 `json:"total"`
	Allocated int64 `json:"allocated"`
}

// Report is the whole node, as GET /system/capacity returns it.
type Report struct {
	VCPUs       Metric `json:"vcpus"`
	MemoryBytes Metric `json:"memory_bytes"`
	DiskBytes   Metric `json:"disk_bytes"`
}

// Request is what one instance would take.
type Request struct {
	VCPUs       int
	MemoryBytes int64
	DiskBytes   int64
}

// BytesPerGiB converts the GiB the API speaks into bytes.
const BytesPerGiB = 1 << 30

// BytesPerMiB converts the MiB the configuration speaks into bytes.
const BytesPerMiB = 1 << 20

// Build turns an instance type and a requested root disk into a Request.
func Build(vcpus int, memoryMiB, rootDiskGB int64) Request {
	return Request{
		VCPUs:       vcpus,
		MemoryBytes: memoryMiB * BytesPerMiB,
		DiskBytes:   rootDiskGB * BytesPerGiB,
	}
}

// Compute splits a node into what it has and what is already promised, as
// GET /system/capacity reports it. The rules are documented in
// 00-architecture.md §6.
func Compute(total, allocated hypervisor.Capacity, limits Limits) Report {
	overcommit := limits.VCPUOvercommit
	if overcommit < 1 {
		overcommit = 1
	}

	return Report{
		VCPUs: Metric{
			Total:     int64(total.VCPUs * overcommit),
			Allocated: int64(allocated.VCPUs),
		},
		MemoryBytes: Metric{
			Total:     total.MemoryBytes,
			Allocated: allocated.MemoryBytes,
		},
		DiskBytes: Metric{
			Total:     total.DiskBytes,
			Allocated: allocated.DiskBytes,
		},
	}
}

// Free returns what Compute left, per metric. Results are clamped at zero: a
// node that is already oversubscribed reports nothing free rather than a
// negative number the console would have to special-case. The memory reserve is
// subtracted here rather than from the total so the reported total stays the
// host's real memory: the reserve is headroom, not a smaller host.
func Free(report Report, limits Limits) hypervisor.Capacity {
	memory := report.MemoryBytes.Total - report.MemoryBytes.Allocated - limits.MemoryReserveMiB*BytesPerMiB
	vcpus := report.VCPUs.Total - report.VCPUs.Allocated
	disk := report.DiskBytes.Total - report.DiskBytes.Allocated

	return hypervisor.Capacity{
		VCPUs:       int(clamp(vcpus)),
		MemoryBytes: clamp(memory),
		DiskBytes:   clamp(disk),
	}
}

// Check reports whether a node with free capacity left can take want. It returns
// a service.ErrQuota error naming the short resource, which the api layer maps
// to 429 with the quota_exceeded code of docs/design/01-api.md.
//
// Disk is compared exactly, because allocated disk is the sum of the root disks
// that were promised rather than the bytes a thin-provisioned overlay happens to
// be using today. The node therefore cannot promise more disk than its pool can
// ever hold, which is the failure an operator would have to clean up by hand.
func Check(free hypervisor.Capacity, want Request, limits Limits) error {
	switch {
	case want.VCPUs > free.VCPUs:
		return fmt.Errorf("%w: %d vCPUs requested, %d available on this node (KUMO_VCPU_OVERCOMMIT=%d)",
			service.ErrQuota, want.VCPUs, free.VCPUs, limits.VCPUOvercommit)
	case want.MemoryBytes > free.MemoryBytes:
		return fmt.Errorf("%w: %d MiB of memory requested, %d MiB available on this node (KUMO_MEMORY_RESERVE_MIB=%d)",
			service.ErrQuota, want.MemoryBytes/BytesPerMiB, free.MemoryBytes/BytesPerMiB,
			limits.MemoryReserveMiB)
	case want.DiskBytes > free.DiskBytes:
		return fmt.Errorf("%w: a %d GiB root disk was requested, %d GiB is free in the instances pool",
			service.ErrQuota, want.DiskBytes/BytesPerGiB, free.DiskBytes/BytesPerGiB)
	}
	return nil
}

// clamp keeps a free figure from going negative.
func clamp(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
