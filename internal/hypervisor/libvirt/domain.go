package libvirt

import (
	"context"
	"fmt"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// Start, Shutdown, Reboot, Reset, ConsoleOutput and Events arrive with the
// lifecycle milestone (M4.4). Until then they refuse rather than pretend, so a
// caller that reaches them gets hypervisor.ErrNotImplemented and degrades instead
// of corrupting the host.
var _ hypervisor.Hypervisor = (*Client)(nil)

func (c *Client) Start(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Shutdown(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Reboot(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Reset(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) ConsoleOutput(context.Context, string, int) ([]byte, error) {
	return nil, hypervisor.ErrNotImplemented
}

// Capacity reports what the host has: the vCPUs and memory of the node, and
// the size of the instances pool.
//
// It is deliberately the totals and nothing else. What is already taken is
// summed from the instances table by the service layer, because a terminated
// instance keeps its row for history while holding nothing, and libvirt cannot
// tell the difference between an overlay on top of an image and a stale one.
func (c *Client) Capacity(ctx context.Context) (hypervisor.Capacity, error) {
	var out hypervisor.Capacity

	if err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		_, memory, cpus, _, _, _, _, _, err := conn.NodeGetInfo()
		if err != nil {
			return fmt.Errorf("read the node info: %w", err)
		}

		out.VCPUs = int(cpus)
		// NodeGetInfo reports memory in KiB.
		out.MemoryBytes = int64(memory) * 1024
		return nil
	}); err != nil {
		return hypervisor.Capacity{}, err
	}

	pool := c.recordedInfra().InstancesPool
	if pool == "" {
		// EnsureInfra has not run, so there is no pool to measure yet. Zero disk
		// reads as a node that cannot take a disk, which is the safe direction.
		return out, nil
	}

	if err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		handle, err := conn.StoragePoolLookupByName(pool)
		if err != nil {
			return fmt.Errorf("look the instances pool %q up: %w", pool, err)
		}
		_, capacity, _, _, err := conn.StoragePoolGetInfo(handle)
		if err != nil {
			return fmt.Errorf("read the instances pool %q: %w", pool, err)
		}
		out.DiskBytes = int64(capacity)
		return nil
	}); err != nil {
		return hypervisor.Capacity{}, err
	}

	return out, nil
}

func (c *Client) Events(context.Context) (<-chan hypervisor.DomainEvent, error) {
	return nil, hypervisor.ErrNotImplemented
}
