package store

import (
	"context"
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// AllocatedCapacity sums the vCPUs, memory and root disk of every instance that
// is not terminated, so a caller can compare it against the host totals without
// walking the instances table itself. It is the allocator half of
// service/system.Capacity.
func (s *Store) AllocatedCapacity(ctx context.Context) (hypervisor.Capacity, error) {
	row, err := s.Queries.AllocatedCapacity(ctx)
	if err != nil {
		return hypervisor.Capacity{}, fmt.Errorf("store: sum the allocated capacity: %w", err)
	}
	return hypervisor.Capacity{
		VCPUs:       int(row.Vcpus),
		MemoryBytes: row.MemoryBytes,
		DiskBytes:   row.DiskBytes,
	}, nil
}
