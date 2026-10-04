package libvirt

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

const (
	poolTypeDir = "dir"
)

// EnsureInfra creates storage pools and network idempotently. The names it used
// are recorded on the client, because the volume calls need to know which pool
// holds images.
func (c *Client) EnsureInfra(ctx context.Context, cfg hypervisor.InfraConfig) error {
	c.setInfra(cfg)

	if err := c.ensurePool(ctx, cfg.ImagesPool, filepath.Join(cfg.DataDir, "pools", "images")); err != nil {
		return fmt.Errorf("ensure images pool: %w", err)
	}
	if err := c.ensurePool(ctx, cfg.InstancesPool, filepath.Join(cfg.DataDir, "pools", "instances")); err != nil {
		return fmt.Errorf("ensure instances pool: %w", err)
	}
	if err := c.ensureNetwork(ctx, cfg.Network); err != nil {
		return fmt.Errorf("ensure network: %w", err)
	}
	return nil
}

func (c *Client) ensurePool(ctx context.Context, name, path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}

	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		pool, err := conn.StoragePoolLookupByName(name)
		if err == nil {
			// Ensure it's active
			if err := conn.StoragePoolCreate(pool, 0); err != nil {
				// Ignore if already active
				if !strings.Contains(err.Error(), "already active") {
					return err
				}
			}
			return nil
		}

		// Create pool XML
		xml := fmt.Sprintf(`<pool type='%s'><name>%s</name><target><path>%s</path></target></pool>`, poolTypeDir, name, path)
		pool, err = conn.StoragePoolDefineXML(xml, 0)
		if err != nil {
			return err
		}
		if err := conn.StoragePoolCreate(pool, 0); err != nil {
			if !strings.Contains(err.Error(), "already active") {
				return err
			}
		}
		return conn.StoragePoolSetAutostart(pool, 1)
	})
}
