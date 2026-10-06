package libvirt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
)

// bridgeName derives the bridge interface of a network from its name. A bridge
// name has to fit Linux's IFNAMSIZ (15 characters); libvirt answers "Numerical
// result out of range" for a longer one. The default networks are short and
// keep a readable bridge (kumo-default -> virbr-default); a longer name is
// shortened and hashed so the interface stays unique instead of colliding on a
// shared prefix.
func bridgeName(networkName string) string {
	suffix := strings.TrimPrefix(networkName, "kumo-")
	if len("virbr-")+len(suffix) <= 15 {
		return "virbr-" + suffix
	}
	sum := sha256.Sum256([]byte(networkName))
	return "virbr-" + suffix[:4] + "-" + hex.EncodeToString(sum[:2])
}

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
		xml := fmt.Sprintf(`<network><name>%s</name><forward mode='nat'><nat><port start='1024' end='65535'/></nat></forward><bridge name='%s' stp='on' delay='0'/><ip address='192.168.100.1' netmask='255.255.255.0'><dhcp><range start='192.168.100.10' end='192.168.100.250'/></dhcp></ip></network>`,
			name, bridgeName(name))
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
