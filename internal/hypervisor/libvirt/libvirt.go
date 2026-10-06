package libvirt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
)

var (
	// ErrNoKVM is returned when /dev/kvm is not available.
	ErrNoKVM = errors.New("libvirt: /dev/kvm not available")
	// ErrLibvirtUnavailable is returned when libvirt is not reachable.
	ErrLibvirtUnavailable = errors.New("libvirt: unavailable")
)

const (
	defaultSocketURI = "unix:///var/run/libvirt/libvirt-sock"
	defaultDriverURI = "qemu:///system"
)

// Client wraps the reconnecting libvirt connection.
type Client struct {
	// uri is the socket address from config (e.g. unix:///var/run/libvirt/
	// libvirt-sock), not a libvirt driver URI.
	uri         string
	reconnector *Reconnector
	logger      *slog.Logger

	// infra remembers the pool and network names EnsureInfra created, so the
	// volume calls can find the images pool without looking it up by
	// convention. It is empty until EnsureInfra runs.
	infraMu   sync.RWMutex
	infraConf hypervisor.InfraConfig
}

// recordedInfra returns the infrastructure configuration EnsureInfra created.
func (c *Client) recordedInfra() hypervisor.InfraConfig {
	c.infraMu.RLock()
	defer c.infraMu.RUnlock()
	return c.infraConf
}

// setInfra records the infrastructure configuration EnsureInfra created.
func (c *Client) setInfra(cfg hypervisor.InfraConfig) {
	c.infraMu.Lock()
	defer c.infraMu.Unlock()
	c.infraConf = cfg
}

// New creates a new libvirt client.
func New(logger *slog.Logger, uri string) (*Client, error) {
	if uri == "" {
		uri = defaultSocketURI
	}

	socketPath := strings.TrimPrefix(uri, "unix://")

	c := &Client{
		uri:         uri,
		logger:      logger,
		reconnector: NewReconnector(defaultDriverURI, socketPath),
	}

	if logger == nil {
		c.logger = slog.Default()
	}

	return c, nil
}

// Connect establishes the connection.
func (c *Client) Connect(ctx context.Context) error {
	return c.reconnector.Connect(ctx)
}

// Close closes the connection.
func (c *Client) Close() error {
	return c.reconnector.Close()
}

// HasKVM implements system.KVMReporter on the Client, so /system/info can
// report that the daemon is up while no guest can boot.
func (c *Client) HasKVM() bool { return HasKVM() }

// HasKVM checks if /dev/kvm exists and is accessible.
func HasKVM() bool {
	info, err := os.Stat("/dev/kvm")
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeDevice != 0
}

// Ping reports whether the libvirt daemon answers. A missing /dev/kvm is not a
// ping failure: 04-libvirt-integration.md §5 wants it surfaced as `kvm: false`
// in /system/info, with launches the thing that fail as `unavailable`. Readiness
// only needs the daemon.
func (c *Client) Ping(ctx context.Context) error {
	conn, err := c.reconnector.ensureConnected(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLibvirtUnavailable, err)
	}

	if !conn.IsConnected() {
		return ErrLibvirtUnavailable
	}
	return nil
}

// withConnection executes fn with a connected libvirt client, handling reconnects.
func (c *Client) withConnection(ctx context.Context, fn func(*golibvirt.Libvirt) error) error {
	conn, err := c.reconnector.ensureConnected(ctx)
	if err != nil {
		return err
	}

	err = fn(conn)
	if err != nil && shouldReconnect(err) {
		if cerr := c.reconnector.Close(); cerr != nil {
			c.logger.Debug("closing stale libvirt connection", "error", cerr)
		}
		conn, err = c.reconnector.ensureConnected(ctx)
		if err != nil {
			return err
		}
		return fn(conn)
	}
	return err
}
