package libvirt

import (
	"context"
	"encoding/xml"
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
//
// It asks libvirt rather than looking for /dev/kvm, because kumod is not the
// process that opens the device: qemu is, under libvirtd. The packaged unit
// runs kumod with PrivateDevices=yes, so a stat of /dev/kvm from here says
// "absent" on every packaged node however healthy the host is. libvirt's own
// capabilities list a `kvm` domain type exactly when it can start the
// type="kvm" domains domainxml builds, which is the question being asked.
func (c *Client) HasKVM(ctx context.Context) (bool, error) {
	var caps []byte
	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		var err error
		caps, err = conn.Capabilities()
		return err
	})
	if err != nil {
		return false, fmt.Errorf("%w: capabilities: %w", ErrLibvirtUnavailable, err)
	}
	return capabilitiesOfferKVM(caps)
}

// libvirtCapabilities is the part of libvirt's capabilities document HasKVM
// reads: per guest architecture, the domain types libvirt can run.
type libvirtCapabilities struct {
	Guests []struct {
		OSType string `xml:"os_type"`
		Arch   struct {
			Name    string `xml:"name,attr"`
			Domains []struct {
				Type string `xml:"type,attr"`
			} `xml:"domain"`
		} `xml:"arch"`
	} `xml:"guest"`
}

// capabilitiesOfferKVM reports whether any hvm guest in a libvirt capabilities
// document can run as a kvm domain. A host without usable KVM still lists its
// guests, with only the `qemu` (software emulation) domain type.
func capabilitiesOfferKVM(doc []byte) (bool, error) {
	var caps libvirtCapabilities
	if err := xml.Unmarshal(doc, &caps); err != nil {
		return false, fmt.Errorf("libvirt: parse capabilities: %w", err)
	}
	for _, guest := range caps.Guests {
		if guest.OSType != "hvm" {
			continue
		}
		for _, domain := range guest.Arch.Domains {
			if domain.Type == "kvm" {
				return true, nil
			}
		}
	}
	return false, nil
}

// DevKVMPresent reports whether /dev/kvm exists as a device node in this
// process's view of /dev. It is the host check `kumod doctor` makes, run by an
// operator outside the service's sandbox. It is not what /system/info reports
// (see Client.HasKVM), and it does not test permissions.
func DevKVMPresent() bool {
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
			c.logger.DebugContext(ctx, "closing stale libvirt connection", "error", cerr)
		}
		conn, err = c.reconnector.ensureConnected(ctx)
		if err != nil {
			return err
		}
		return fn(conn)
	}
	return err
}
