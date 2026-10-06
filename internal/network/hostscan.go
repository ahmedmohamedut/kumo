package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"
)

// ErrMalformedRouteTable is returned when /proc/net/route cannot be interpreted,
// which on a Linux host with a sane kernel should not happen.
var ErrMalformedRouteTable = errors.New("network: /proc/net/route is malformed")

// hostRoute is one entry of the host's IPv4 routing table.
type hostRoute struct {
	iface  string
	prefix netip.Prefix
}

// hostIface is one address-bearing interface of the host.
type hostIface struct {
	name   string
	prefix netip.Prefix
}

// ownBridge reports whether a host interface is a bridge a Kumo-managed
// libvirt network lives on. Those subnets are already tracked by the vpcs
// table, so the service checks a new VPC against existing VPC rows instead of
// letting a host scan that would always see the old VPCs reject the new ones.
func ownBridge(name string) bool {
	return strings.HasPrefix(name, "kmb") || strings.HasPrefix(name, "virbr-")
}

// HostCollision reports the first host route or interface subnet that overlaps
// prefix, or "" when none does. Kumo's own bridges and the loopback are
// excluded, the loopback because nothing Kumo accepts (RFC 1918, /16../28)
// can overlap it anyway and 127.0.0.0/8 would only make the error message
// confusing.
//
// The default route (0.0.0.0/0) is skipped too: a NAT host always has one, and
// rejecting every VPC CIDR because it overlaps the universe would make VPC
// creation impossible. The point of the check is the connected routes of real
// LANs, which a VPC CIDR must not hijack.
func HostCollision(prefix netip.Prefix) (string, error) {
	routes, err := readHostRoutes()
	if err != nil {
		return "", err
	}
	subnets, err := hostInterfaceSubnets()
	if err != nil {
		return "", err
	}
	return firstHostCollision(prefix, routes, subnets), nil
}

// firstHostCollision compares a prefix against collected host routes and
// interface subnets. It is pure, so the exclusion rules and the wording of a
// collision can be tested without reading the real routing table.
func firstHostCollision(prefix netip.Prefix, routes []hostRoute, subnets []hostIface) string {
	for _, route := range routes {
		if ownBridge(route.iface) || route.iface == "lo" {
			continue
		}
		if prefix.Overlaps(route.prefix) {
			return fmt.Sprintf("the host route %s via %s", route.prefix, route.iface)
		}
	}
	for _, subnet := range subnets {
		if ownBridge(subnet.name) || subnet.name == "lo" {
			continue
		}
		if prefix.Overlaps(subnet.prefix) {
			return fmt.Sprintf("the subnet %s on %s", subnet.prefix, subnet.name)
		}
	}
	return ""
}

// readHostRoutes parses the IPv4 routing table. /proc/net/route stores the
// destination and mask as 8 hex characters, each byte of the address least
// significant first: 192.168.0.0 shows up as 0000A8C0 and its /24 mask as
// 00FFFFFF.
func readHostRoutes() ([]hostRoute, error) {
	data, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return nil, fmt.Errorf("network: read the host routes: %w", err)
	}
	return parseRouteTable(data)
}

// parseRouteTable turns a /proc/net/route dump into host routes.
func parseRouteTable(data []byte) ([]hostRoute, error) {
	var routes []hostRoute
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		if fields[0] == "Iface" || fields[1] == "Destination" {
			continue // header line
		}
		dest, err := leHexAddr(fields[1])
		if err != nil {
			return nil, fmt.Errorf("%w: destination %q", ErrMalformedRouteTable, fields[1])
		}
		mask, err := leHexAddr(fields[7])
		if err != nil {
			return nil, fmt.Errorf("%w: mask %q", ErrMalformedRouteTable, fields[7])
		}
		// The default route covers everything and is skipped here, the same way
		// HostCollision skips it: it is the lane to the Internet, not a LAN the
		// VPC could shadow.
		if dest.IsUnspecified() {
			continue
		}
		bits := maskBits(mask)
		routes = append(routes, hostRoute{
			iface:  fields[0],
			prefix: netip.PrefixFrom(dest, bits).Masked(),
		})
	}
	return routes, nil
}

// hostInterfaceSubnets collects the IPv4 subnet of every non-loopback interface
// that is not one of Kumo's bridges. Zones without a route entry (unusual, but
// legal) are caught here rather than only by the routing table.
func hostInterfaceSubnets() ([]hostIface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("network: list the host interfaces: %w", err)
	}

	var out []hostIface
	for _, iface := range ifaces {
		if ownBridge(iface.Name) || iface.Name == "lo" {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil {
				continue
			}
			bits, _ := ipnet.Mask.Size()
			var a [4]byte
			copy(a[:], ip)
			out = append(out, hostIface{
				name:   iface.Name,
				prefix: netip.PrefixFrom(netip.AddrFrom4(a), bits).Masked(),
			})
		}
	}
	return out, nil
}

// leHexAddr parses the little-endian hex form of an IPv4 address the routing
// table uses.
func leHexAddr(s string) (netip.Addr, error) {
	if len(s) != 8 {
		return netip.Addr{}, fmt.Errorf("%w: %q is not eight hex digits", ErrMalformedRouteTable, s)
	}
	var address [4]byte
	for i := 0; i < 4; i++ {
		value, err := hexNibblePair(s[2*i : 2*i+2])
		if err != nil {
			return netip.Addr{}, err
		}
		// Pair 0 is the least significant byte of the address.
		address[3-i] = value
	}
	return netip.AddrFrom4(address), nil
}

// hexNibblePair parses one byte as two hex digits.
func hexNibblePair(pair string) (byte, error) {
	var value byte
	for i := 0; i < 2; i++ {
		value <<= 4
		ch := pair[i]
		switch {
		case ch >= '0' && ch <= '9':
			value |= ch - '0'
		case ch >= 'a' && ch <= 'f':
			value |= ch - 'a' + 10
		case ch >= 'A' && ch <= 'F':
			value |= ch - 'A' + 10
		default:
			return 0, fmt.Errorf("%w: %q is not hex", ErrMalformedRouteTable, pair)
		}
	}
	return value, nil
}

// maskBits counts the set bits of a dotted-quad-style netmask given as an
// address.
func maskBits(mask netip.Addr) int {
	b := mask.As4()
	ones := 0
	for _, octet := range b {
		for octet != 0 {
			ones += int(octet & 1)
			octet >>= 1
		}
	}
	return ones
}
