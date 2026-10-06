// Package network validates and manipulates the IPv4 blocks Kumo hands out to
// VPCs and subnets, and scans the host's own routes and interfaces for
// collisions. It is a pure helper package: the service layer owns allocation
// and persistence, this package only decides what is valid and what is taken.
//
// Docs/design/01-api.md §VPCs and §Subnets are the rules implemented here.
package network

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// MinPrefixBits and MaxPrefixBits bound the prefixes Kumo accepts for a VPC or
// subnet CIDR: enough addresses for useful guests, small enough to stay sane on
// one bridge.
const (
	MinPrefixBits = 16
	MaxPrefixBits = 28
)

// rfc1918 holds the three private blocks a VPC CIDR must sit inside.
var rfc1918 = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// Sentinel errors for validation failures. The service maps them all to a 400;
// the distinction exists so a message can name the rule that was broken.
var (
	// ErrInvalidCIDR wraps every CIDR that cannot back a network.
	ErrInvalidCIDR = errors.New("network: invalid CIDR")
	// ErrNotRFC1918 is returned when a VPC CIDR is valid but not private space.
	ErrNotRFC1918 = errors.New("network: the VPC CIDR is not RFC 1918")
	// ErrNotContained is returned when a subnet CIDR leaves its VPC.
	ErrNotContained = errors.New("network: the subnet CIDR is not contained in the VPC CIDR")
)

// ValidateVPCCIDR parses a VPC CIDR and refuses everything that could not back
// a working libvirt network: a prefix that is not private RFC 1918 space, one
// outside /16../28, or one that does not name a network address.
func ValidateVPCCIDR(cidr string) (netip.Prefix, error) {
	prefix, err := validateCIDR(cidr)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !isRFC1918(prefix) {
		return netip.Prefix{}, fmt.Errorf("%w: %s is not in 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16",
			ErrNotRFC1918, prefix)
	}
	return prefix, nil
}

// ValidateSubnetCIDR parses a subnet CIDR and refuses anything that could not
// partition its parent VPC: a prefix outside /16../28, one that is not fully
// contained in the VPC CIDR, or one that names the same range as the VPC
// itself. Sibling overlap is a separate check (Overlaps), because it needs the
// other subnets of the VPC.
func ValidateSubnetCIDR(cidr string, vpc netip.Prefix) (netip.Prefix, error) {
	prefix, err := validateCIDR(cidr)
	if err != nil {
		return netip.Prefix{}, err
	}
	if !Contains(vpc, prefix) {
		return netip.Prefix{}, fmt.Errorf("%w: the subnet CIDR %s is not fully contained in the VPC CIDR %s",
			ErrNotContained, prefix, vpc)
	}
	if prefix.Bits() == vpc.Bits() {
		// A subnet must sit strictly inside the VPC: the same range as the VPC
		// would put the gateway and the subnet's network address on top of each
		// other.
		return netip.Prefix{}, fmt.Errorf("%w: the subnet CIDR %s must not cover the whole VPC CIDR",
			ErrInvalidCIDR, prefix)
	}
	return prefix, nil
}

// validateCIDR parses a single IPv4 CIDR and applies the rules shared by VPCs
// and subnets.
func validateCIDR(cidr string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: %q is not a CIDR: %w", ErrInvalidCIDR, cidr, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%w: the CIDR %s is not IPv4", ErrInvalidCIDR, prefix)
	}
	if prefix.Masked() != prefix {
		return netip.Prefix{}, fmt.Errorf("%w: the CIDR %s does not name a network address; use %s",
			ErrInvalidCIDR, prefix, prefix.Masked())
	}
	switch {
	case prefix.Bits() < MinPrefixBits:
		return netip.Prefix{}, fmt.Errorf("%w: the CIDR %s is too wide: at most /%d is allowed",
			ErrInvalidCIDR, prefix, MinPrefixBits)
	case prefix.Bits() > MaxPrefixBits:
		return netip.Prefix{}, fmt.Errorf("%w: the CIDR %s is too narrow: at least /%d is required",
			ErrInvalidCIDR, prefix, MaxPrefixBits)
	}
	return prefix, nil
}

// isRFC1918 reports whether prefix sits inside one of the private blocks.
func isRFC1918(prefix netip.Prefix) bool {
	for _, block := range rfc1918 {
		if Contains(block, prefix) {
			return true
		}
	}
	return false
}

// Contains reports whether parent covers every address of child.
func Contains(parent, child netip.Prefix) bool {
	child = child.Masked()
	if !parent.Contains(child.Addr()) {
		return false
	}
	bits := parent.Bits()
	if child.Bits() > bits {
		return true
	}
	return child.Bits() == bits
}

// Overlaps reports whether two prefixes share at least one address, which is
// what makes two sibling subnets of one VPC incompatible.
func Overlaps(a, b netip.Prefix) bool { return a.Overlaps(b) }

// Gateway returns the first usable address of a prefix, the one libvirt puts on
// the bridge and dnsmasq considers its own. The VPC row stores it as
// gateway_ip.
func Gateway(prefix netip.Prefix) netip.Addr {
	return addOffset(prefix.Masked().Addr(), 1)
}

// Netmask returns the dotted-quad netmask of a prefix, e.g. 255.255.0.0 for a
// /16, the form libvirt's <ip netmask='…'/> wants.
func Netmask(prefix netip.Prefix) string {
	return net.IP(net.CIDRMask(prefix.Bits(), 32)).String()
}

// TotalAddresses is the number of IPv4 addresses a prefix covers.
func TotalAddresses(prefix netip.Prefix) int64 {
	return int64(1) << (32 - prefix.Bits())
}

// UsableCount counts the addresses Kumo may hand out: every address of the
// prefix except the network address, the broadcast address, and — when the
// VPC's gateway falls inside this prefix — the gateway itself. The gateway is
// the one address of the VPC that is never for sale.
func UsableCount(prefix netip.Prefix, gateway netip.Addr) int64 {
	count := TotalAddresses(prefix) - 2
	if prefix.Contains(gateway) {
		count--
	}
	return count
}

// ForEachUsable visits every usable address of a prefix, from lowest to
// highest, stopping when visit returns false or the range is exhausted. The
// addresses the VPC reserves (network, broadcast, gateway) are skipped, which
// is exactly the order a lowest-free-address allocator needs.
func ForEachUsable(prefix netip.Prefix, gateway netip.Addr, visit func(addr netip.Addr) bool) {
	base := prefix.Masked().Addr()
	// host counts from 1 to total-2; host 0 is the network address and the last
	// address is the broadcast.
	for host := int64(1); host < TotalAddresses(prefix)-1; host++ {
		addr := addOffset(base, host)
		if addr == gateway {
			continue
		}
		if !visit(addr) {
			return
		}
	}
}

// addOffset returns addr advanced by offset addresses.
func addOffset(addr netip.Addr, offset int64) netip.Addr {
	b := addr.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += uint32(offset)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
