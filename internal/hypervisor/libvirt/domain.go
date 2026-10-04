package libvirt

import (
	"context"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

// The volume and domain halves of the backend arrive with the image and
// instance milestones. Until then the operations refuse rather than pretend,
// so a caller that reaches them gets hypervisor.ErrNotImplemented and degrades
// instead of corrupting the host.
var _ hypervisor.Hypervisor = (*Client)(nil)

func (c *Client) DefineAndStart(context.Context, hypervisor.DomainSpec) (hypervisor.DomainInfo, error) {
	return hypervisor.DomainInfo{}, hypervisor.ErrNotImplemented
}

func (c *Client) Start(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Shutdown(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Destroy(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Reboot(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Reset(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) Undefine(context.Context, string) error { return hypervisor.ErrNotImplemented }

func (c *Client) GetDomain(context.Context, string) (hypervisor.DomainInfo, error) {
	return hypervisor.DomainInfo{}, hypervisor.ErrNotImplemented
}

func (c *Client) ListDomains(context.Context) ([]hypervisor.DomainInfo, error) {
	return nil, hypervisor.ErrNotImplemented
}

func (c *Client) ConsoleOutput(context.Context, string, int) ([]byte, error) {
	return nil, hypervisor.ErrNotImplemented
}

// Capacity reports the host totals from libvirt's node info, which is enough for
// GET /system/capacity in M4.1.
func (c *Client) Capacity(ctx context.Context) (hypervisor.Capacity, error) {
	return hypervisor.Capacity{}, hypervisor.ErrNotImplemented
}

func (c *Client) Events(context.Context) (<-chan hypervisor.DomainEvent, error) {
	return nil, hypervisor.ErrNotImplemented
}

// CreateRootDisk arrives with the instance launch workflow (M4.3).
func (c *Client) CreateRootDisk(context.Context, hypervisor.RootDiskSpec) error {
	return hypervisor.ErrNotImplemented
}

// CreateSeedISO arrives with the cloud-init builder (M4.2).
func (c *Client) CreateSeedISO(context.Context, string, hypervisor.CloudInit) error {
	return hypervisor.ErrNotImplemented
}
