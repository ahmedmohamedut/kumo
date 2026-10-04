package libvirt

import (
	"context"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

func (c *Client) ensureNetwork(ctx context.Context, name string) error {
	return c.withConnection(ctx, func(conn *golibvirt.Libvirt) error {
		net, err := conn.NetworkLookupByName(name)
		if err == nil {
			if err := conn.NetworkCreate(net); err != nil {
				if !strings.Contains(err.Error(), "already active") {
					return err
				}
			}
			return nil
		}

		// Create default NAT network as per design
		xml := fmt.Sprintf(`<network><name>%s</name><forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward><bridge name='virbr-%s' stp='on' delay='0'/><ip address='192.168.100.1' netmask='255.255.255.0'><dhcp><range start='192.168.100.10' end='192.168.100.250'/></dhcp></ip></network>`,
			name, strings.TrimPrefix(name, "kumo-"))
		net, err = conn.NetworkDefineXML(xml)
		if err != nil {
			return err
		}
		if err := conn.NetworkCreate(net); err != nil {
			if !strings.Contains(err.Error(), "already active") {
				return err
			}
		}
		return conn.NetworkSetAutostart(net, 1)
	})
}
