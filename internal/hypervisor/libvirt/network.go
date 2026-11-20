package libvirt

import (
	"context"
	"errors"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt/networkxml"
)

// DefineNetwork defines a VPC's libvirt network from its spec. Definition only:
// the network starts inactive and StartNetwork brings it up, which is how a VPC
// create task proceeds (docs/design/04-libvirt-integration.md §6).
func (c *Client) DefineNetwork(ctx context.Context, spec hypervisor.NetworkSpec) error {
	if err := requireNetworkName(spec.Name); err != nil {
		return err
	}
	doc, err := networkxml.Build(spec)
	if err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		_, err := conn.NetworkLookupByName(spec.Name)
		switch {
		case err == nil:
			return fmt.Errorf("the network %q: %w", spec.Name, hypervisor.ErrAlreadyExists)
		case !isNoNetwork(err):
			return fmt.Errorf("look the network %q up: %w", spec.Name, err)
		}

		net, err := conn.NetworkDefineXML(string(doc))
		if err != nil {
			return fmt.Errorf("define the network %q: %w", spec.Name, err)
		}
		if err := conn.NetworkSetAutostart(net, 1); err != nil {
			return fmt.Errorf("autostart the network %q: %w", spec.Name, err)
		}
		return nil
	})
}

// StartNetwork activates a defined network. Starting an active one fails with
// hypervisor.ErrInvalidState, the same answer libvirt's virNetworkCreate gives,
// so a requeued create task that already started its network adopts it instead
// of failing.
func (c *Client) StartNetwork(ctx context.Context, name string) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		if err := conn.NetworkCreate(net); err != nil {
			if isOperationInvalid(err) || strings.Contains(err.Error(), "already active") {
				return fmt.Errorf("the network %q is already active: %w", name, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("start the network %q: %w", name, err)
		}
		return nil
	})
}

// DestroyNetwork stops an active network. Stopping one that is not active is an
// invalid-state error, like libvirt's.
func (c *Client) DestroyNetwork(ctx context.Context, name string) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		if err := conn.NetworkDestroy(net); err != nil {
			if isOperationInvalid(err) {
				return fmt.Errorf("the network %q is not active: %w", name, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("destroy the network %q: %w", name, err)
		}
		return nil
	})
}

// UndefineNetwork removes a network's definition. An active network cannot be
// undefined; the delete task destroys it first.
func (c *Client) UndefineNetwork(ctx context.Context, name string) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		if err := conn.NetworkUndefine(net); err != nil {
			if isOperationInvalid(err) {
				return fmt.Errorf("the network %q is still active: %w", name, hypervisor.ErrInvalidState)
			}
			return fmt.Errorf("undefine the network %q: %w", name, err)
		}
		return nil
	})
}

// GetNetwork reports the state of a network, read back from the daemon the way
// adoption needs it: the name, bridge, CIDR, gateway, mode and static hosts a
// network that already exists has, plus whether it is active.
func (c *Client) GetNetwork(ctx context.Context, name string) (hypervisor.NetworkInfo, error) {
	var info hypervisor.NetworkInfo
	err := c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		desc, err := conn.NetworkGetXMLDesc(net, 0)
		if err != nil {
			return fmt.Errorf("describe the network %q: %w", name, err)
		}
		parsed, err := networkxml.Parse([]byte(desc))
		if err != nil {
			return fmt.Errorf("read the description of the network %q: %w", name, err)
		}
		active, err := conn.NetworkIsActive(net)
		if err != nil {
			return fmt.Errorf("check whether the network %q is active: %w", name, err)
		}
		parsed.Active = active == 1
		info = parsed
		return nil
	})
	return info, err
}

// AddDHCPHost installs a static <host mac ip name> entry in a network's dhcp
// block through virNetworkUpdate, the only way Kumo tells dnsmasq what it has
// allocated (docs/design/04-libvirt-integration.md §6.4).
func (c *Client) AddDHCPHost(ctx context.Context, name string, host hypervisor.DHCPHost) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	fragment, err := networkxml.HostXML(host)
	if err != nil {
		return err
	}
	return c.updateDHCPHost(ctx, name, host.MAC, golibvirt.NetworkUpdateCommandAddLast, string(fragment))
}

// DeleteDHCPHost removes a static dhcp host entry by MAC, which is how the
// terminate workflow frees the lease (docs/design/04-libvirt-integration.md §3).
func (c *Client) DeleteDHCPHost(ctx context.Context, name, mac string) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	fragment, err := networkxml.HostXML(hypervisor.DHCPHost{MAC: mac})
	if err != nil {
		return err
	}
	return c.updateDHCPHost(ctx, name, mac, golibvirt.NetworkUpdateCommandDelete, string(fragment))
}

// updateDHCPHost runs one virNetworkUpdate against the dhcp host section of a
// network, live and persisted, so the running dnsmasq and the stored definition
// agree on what Kumo allocated.
func (c *Client) updateDHCPHost(ctx context.Context, name, mac string, command golibvirt.NetworkUpdateCommand, fragment string) error {
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		flags := golibvirt.NetworkUpdateAffectLive | golibvirt.NetworkUpdateAffectConfig
		if err := conn.NetworkUpdate(net, uint32(command), uint32(golibvirt.NetworkSectionIPDhcpHost), -1, fragment, flags); err != nil {
			return fmt.Errorf("update the dhcp hosts of the network %q for %s: %w", name, mac, err)
		}
		return nil
	})
}

// AddDNSHost adds a DNS record to a network, live and persisted, so the running
// dnsmasq serves it at once and a restarted one still does. A record for the
// same address already present is ErrAlreadyExists, which a requeued task
// adopts.
func (c *Client) AddDNSHost(ctx context.Context, name string, host hypervisor.DNSHost) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	fragment, err := networkxml.DNSHostXML(host)
	if err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		if present, err := dnsHostPresent(conn, net, host.IP); err != nil {
			return err
		} else if present {
			return fmt.Errorf("the dns host %s on the network %q: %w", host.IP, name, hypervisor.ErrAlreadyExists)
		}
		flags := golibvirt.NetworkUpdateAffectLive | golibvirt.NetworkUpdateAffectConfig
		if err := conn.NetworkUpdate(net, uint32(golibvirt.NetworkUpdateCommandAddLast),
			uint32(golibvirt.NetworkSectionDNSHost), -1, string(fragment), flags); err != nil {
			return fmt.Errorf("add the dns host %s to the network %q: %w", host.IP, name, err)
		}
		return nil
	})
}

// DeleteDNSHost removes a DNS record. One that is already gone is
// ErrNotFound, which a requeued delete tolerates.
func (c *Client) DeleteDNSHost(ctx context.Context, name string, host hypervisor.DNSHost) error {
	if err := requireNetworkName(name); err != nil {
		return err
	}
	fragment, err := networkxml.DNSHostXML(host)
	if err != nil {
		return err
	}
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := lookupNetwork(conn, name)
		if err != nil {
			return err
		}
		if present, err := dnsHostPresent(conn, net, host.IP); err != nil {
			return err
		} else if !present {
			return fmt.Errorf("the dns host %s on the network %q: %w", host.IP, name, hypervisor.ErrNotFound)
		}
		flags := golibvirt.NetworkUpdateAffectLive | golibvirt.NetworkUpdateAffectConfig
		if err := conn.NetworkUpdate(net, uint32(golibvirt.NetworkUpdateCommandDelete),
			uint32(golibvirt.NetworkSectionDNSHost), -1, string(fragment), flags); err != nil {
			return fmt.Errorf("delete the dns host %s from the network %q: %w", host.IP, name, err)
		}
		return nil
	})
}

// dnsHostPresent reads the network back and reports whether it holds a DNS
// record for ip: libvirt's own "already exists" is a generic error, and this
// is the reading both backends agree on.
func dnsHostPresent(conn *golibvirt.Libvirt, net golibvirt.Network, ip string) (bool, error) {
	doc, err := conn.NetworkGetXMLDesc(net, 0)
	if err != nil {
		return false, fmt.Errorf("read the network %q: %w", net.Name, err)
	}
	info, err := networkxml.Parse([]byte(doc))
	if err != nil {
		return false, err
	}
	for _, h := range info.DNSHosts {
		if h.IP == ip {
			return true, nil
		}
	}
	return false, nil
}

// lookupNetwork finds a network by name, mapping libvirt's "no such network"
// onto hypervisor.ErrNotFound.
func lookupNetwork(conn *golibvirt.Libvirt, name string) (golibvirt.Network, error) {
	net, err := conn.NetworkLookupByName(name)
	if err != nil {
		if isNoNetwork(err) {
			return golibvirt.Network{}, fmt.Errorf("the network %q: %w", name, hypervisor.ErrNotFound)
		}
		return golibvirt.Network{}, fmt.Errorf("look the network %q up: %w", name, err)
	}
	return net, nil
}

// isNoNetwork reports a libvirt "no such network" error, the network sibling of
// isNoDomain: go-libvirt's IsNotFound only matches ERR_NO_DOMAIN.
func isNoNetwork(err error) bool {
	return checkErrorNumber(err, uint32(golibvirt.ErrNoNetwork))
}

// checkErrorNumber unwraps err looking for a libvirt error with the code.
func checkErrorNumber(err error, code uint32) bool {
	for err != nil {
		var libErr golibvirt.Error
		if errors.As(err, &libErr) && libErr.Code == code {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func requireNetworkName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: a network name must not be empty", hypervisor.ErrInvalidSpec)
	}
	return nil
}
