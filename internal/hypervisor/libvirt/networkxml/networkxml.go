// Package networkxml writes and reads the libvirt network definition of a VPC.
//
// It is a pure function over hypervisor.NetworkSpec, the same way domainxml is
// over hypervisor.DomainSpec: the same spec always produces the same XML, and
// nothing here talks to libvirt, so the definition is reviewable in a golden
// file and testable on a machine with no libvirt.
//
// The document is built from encoding/xml structs, never from string
// formatting, so the VPC name that seeds the DNS domain is escaped by the
// encoder. The shape follows docs/design/04-libvirt-integration.md §6:
//
//   - one VPC is one libvirt network on one bridge (kmb<8 hex> of the VPC id);
//   - mode "nat" emits <forward mode='nat'>; "isolated" emits no forward entry;
//   - the <ip> block carries the VPC gateway and an empty <dhcp> element: Kumo
//     is the sole allocator, so there is never a dynamic <range>, and the
//     static hosts arrive later through virNetworkUpdate.
package networkxml

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	ipnet "github.com/ahmedmohamedut/kumo/internal/network"
)

// ErrMalformed wraps every shape that a hand-written or update-created network
// description should not have, e.g. an address block with no netmask. Parse
// treats these blocks as foreign (it skips them), but a caller still needs to
// be able to tell a malformed block from a parse failure.
var ErrMalformed = errors.New("networkxml: malformed network description")

// natPortStart and natPortEnd are the host port range libvirt's NAT forwards
// (docs/design/04-libvirt-integration.md §6.2).
const (
	natPortStart = "1024"
	natPortEnd   = "65535"
)

// network is the root element. The parts that are genuinely optional — the DNS
// domain and the <forward> element of an isolated network — are pointers.
type network struct {
	XMLName xml.Name `xml:"network"`
	Name    string   `xml:"name"`
	Domain  *domain  `xml:"domain,omitempty"`
	Forward *forward `xml:"forward,omitempty"`
	Bridge  bridge   `xml:"bridge"`
	// DNS is read back only: Kumo adds its records with virNetworkUpdate,
	// never in the definition, so a network it defines carries none.
	DNS *dnsElement `xml:"dns,omitempty"`
	IPs []ip        `xml:"ip"`
}

// dnsElement is the <dns> block of a network, as libvirt reports it.
type dnsElement struct {
	Hosts []dnsHostElement `xml:"host"`
}

// dnsHostElement is one <host ip><hostname/></host> DNS record.
type dnsHostElement struct {
	XMLName   xml.Name `xml:"host"`
	IP        string   `xml:"ip,attr"`
	Hostnames []string `xml:"hostname"`
}

// domain is the libvirt DNS domain, seeded by the VPC name.
type domain struct {
	Name string `xml:"name,attr"`
}

// forward is the routing mode of the network. An isolated network has no
// forward element at all, which is how libvirt expresses isolation.
type forward struct {
	Mode string `xml:"mode,attr"`
	NAT  *nat   `xml:"nat,omitempty"`
}

type nat struct {
	Port natPort `xml:"port"`
}

type natPort struct {
	Start string `xml:"start,attr"`
	End   string `xml:"end,attr"`
}

// bridge is the Linux bridge the network lives on. stp is on so a loop in a
// switched topology cannot take the network down.
type bridge struct {
	Name  string `xml:"name,attr"`
	STP   string `xml:"stp,attr"`
	Delay string `xml:"delay,attr"`
}

// ip is one address block of the network. Only the IPv4 block carries the dhcp
// element; a host with IPv6 support may add an <ip family='ipv6'> block that
// Parse skips.
type ip struct {
	Family  string `xml:"family,attr,omitempty"`
	Address string `xml:"address,attr"`
	Netmask string `xml:"netmask,attr"`
	DHCP    *dhcp  `xml:"dhcp,omitempty"`
}

// dhcp is empty by design: dnsmasq serves the static hosts Kumo adds, and no
// range, because Kumo is the sole allocator (§6.4).
type dhcp struct {
	Hosts []hostElement `xml:"host"`
}

// hostElement is a static lease. Name is optional, which is also how a delete
// fragment looks: <host mac='…'/> by itself.
type hostElement struct {
	XMLName xml.Name `xml:"host"`
	MAC     string   `xml:"mac,attr"`
	IP      string   `xml:"ip,attr,omitempty"`
	Name    string   `xml:"name,attr,omitempty"`
}

// Build renders the network XML for a VPC spec.
func Build(spec hypervisor.NetworkSpec) ([]byte, error) {
	if err := Validate(spec); err != nil {
		return nil, err
	}

	mode, err := validateMode(spec.Mode)
	if err != nil {
		return nil, err
	}

	doc := network{
		Name:   spec.Name,
		Bridge: bridge{Name: spec.Bridge, STP: "on", Delay: "0"},
		IPs: []ip{{
			Address: spec.Gateway,
			Netmask: ipnet.Netmask(mustParsePrefix(spec.CIDR)),
			DHCP:    &dhcp{},
		}},
	}
	if spec.Domain != "" {
		doc.Domain = &domain{Name: spec.Domain}
	}
	if mode == hypervisor.NetworkModeNAT {
		doc.Forward = &forward{
			Mode: "nat",
			NAT:  &nat{Port: natPort{Start: natPortStart, End: natPortEnd}},
		}
	}

	out, err := xml.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("networkxml: marshal the network: %w", err)
	}
	return []byte(xml.Header + string(out) + "\n"), nil
}

// HostXML renders the <host> fragment virNetworkUpdate wants when adding or
// removing a static DHCP host. A delete fragment carries only the MAC.
func HostXML(host hypervisor.DHCPHost) ([]byte, error) {
	if host.MAC == "" && host.IP == "" {
		return nil, fmt.Errorf("%w: a dhcp host needs a mac or an ip", hypervisor.ErrInvalidSpec)
	}
	out, err := xml.Marshal(hostElement{MAC: host.MAC, IP: host.IP, Name: host.Name})
	if err != nil {
		return nil, fmt.Errorf("networkxml: marshal the dhcp host: %w", err)
	}
	return out, nil
}

// DNSHostXML renders the <host> fragment virNetworkUpdate wants for a DNS
// record. Every hostname is checked to be a DNS name, so nothing but a name
// reaches the network definition, and marshaling escapes it besides.
func DNSHostXML(host hypervisor.DNSHost) ([]byte, error) {
	if addr, err := netip.ParseAddr(host.IP); err != nil || !addr.Is4() {
		return nil, fmt.Errorf("%w: a dns host needs an IPv4 address, not %q", hypervisor.ErrInvalidSpec, host.IP)
	}
	if len(host.Hostnames) == 0 {
		return nil, fmt.Errorf("%w: a dns host needs a hostname", hypervisor.ErrInvalidSpec)
	}
	for _, name := range host.Hostnames {
		if !isDNSName(name) {
			return nil, fmt.Errorf("%w: %q is not a DNS name", hypervisor.ErrInvalidSpec, name)
		}
	}
	out, err := xml.Marshal(dnsHostElement{IP: host.IP, Hostnames: host.Hostnames})
	if err != nil {
		return nil, fmt.Errorf("networkxml: marshal the dns host: %w", err)
	}
	return out, nil
}

// isDNSName reports whether name is a DNS name of lowercase labels.
func isDNSName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// Parse reads a network description back, the way adoption and the delete path
// need it: a network that libvirt already holds (the adopted default network,
// or a requeued VPC create) is described in XML by the daemon, and this is the
// shape Kumo understands in both backends.
//
// Active is left false: only the caller knows whether the network is running.
func Parse(doc []byte) (hypervisor.NetworkInfo, error) {
	var net network
	if err := xml.Unmarshal(doc, &net); err != nil {
		return hypervisor.NetworkInfo{}, fmt.Errorf("networkxml: parse the network: %w", err)
	}

	info := hypervisor.NetworkInfo{
		Name:   net.Name,
		Bridge: net.Bridge.Name,
		Mode:   parsedMode(net.Forward),
	}
	if net.Domain != nil {
		info.Domain = net.Domain.Name
	}

	for _, block := range net.IPs {
		prefix, err := prefixFrom(block.Address, block.Netmask)
		if err != nil {
			continue // not an IPv4 block with a netmask; not the one we manage
		}
		info.CIDR = prefix.String()
		info.Gateway = block.Address
		if block.DHCP != nil {
			for _, host := range block.DHCP.Hosts {
				info.DHCPHosts = append(info.DHCPHosts, hypervisor.DHCPHost{
					MAC:  host.MAC,
					IP:   host.IP,
					Name: host.Name,
				})
			}
		}
		break
	}
	if net.DNS != nil {
		for _, host := range net.DNS.Hosts {
			info.DNSHosts = append(info.DNSHosts, hypervisor.DNSHost{IP: host.IP, Hostnames: host.Hostnames})
		}
	}
	return info, nil
}

// prefixFrom combines an address and dotted-quad netmask into the CIDR of the
// block.
func prefixFrom(address, netmask string) (netip.Prefix, error) {
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is4() {
		return netip.Prefix{}, fmt.Errorf("%w: %q is not an ipv4 address", ErrMalformed, address)
	}
	ones, err := netmaskBits(netmask)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, ones).Masked(), nil
}

// netmaskBits counts the set bits of a dotted-quad netmask.
func netmaskBits(netmask string) (int, error) {
	parsed, err := netip.ParseAddr(netmask)
	if err != nil || !parsed.Is4() {
		return 0, fmt.Errorf("%w: %q is not a dotted-quad netmask", ErrMalformed, netmask)
	}
	b := parsed.As4()
	ones := 0
	for _, octet := range b {
		for octet != 0 {
			ones += int(octet & 1)
			octet >>= 1
		}
	}
	return ones, nil
}

// Validate refuses a spec that could only produce a network libvirt would
// reject, or one that would create a bridge nobody asked for.
func Validate(spec hypervisor.NetworkSpec) error {
	switch {
	case spec.Name == "":
		return fmt.Errorf("%w: the network needs a name", hypervisor.ErrInvalidSpec)
	case spec.Bridge == "":
		return fmt.Errorf("%w: the network %s needs a bridge interface", hypervisor.ErrInvalidSpec, spec.Name)
	case spec.Gateway == "":
		return fmt.Errorf("%w: the network %s needs a gateway address", hypervisor.ErrInvalidSpec, spec.Name)
	case !validCIDR(spec.CIDR):
		return fmt.Errorf("%w: the network %s needs a valid CIDR, got %q",
			hypervisor.ErrInvalidSpec, spec.Name, spec.CIDR)
	}
	if _, err := validateMode(spec.Mode); err != nil {
		return err
	}
	return nil
}

// validateMode accepts both explicit modes and treats the empty value as the
// API default, nat.
func validateMode(mode hypervisor.NetworkMode) (hypervisor.NetworkMode, error) {
	switch mode {
	case "", hypervisor.NetworkModeNAT:
		return hypervisor.NetworkModeNAT, nil
	case hypervisor.NetworkModeIsolated:
		return hypervisor.NetworkModeIsolated, nil
	}
	return "", fmt.Errorf("%w: the network mode %q is neither nat nor isolated",
		hypervisor.ErrInvalidSpec, mode)
}

// parsedMode reads the forwarding mode back off a network description: an
// absent forward element is the isolated mode.
func parsedMode(fwd *forward) hypervisor.NetworkMode {
	if fwd == nil {
		return hypervisor.NetworkModeIsolated
	}
	return hypervisor.NetworkMode(fwd.Mode)
}

// validCIDR reports whether cidr is an IPv4 prefix between /16 and /28, the
// same bound the service enforces; the builder re-derives the netmask from it,
// so a CIDR that cannot be a netmask or a block would silently produce a
// broken definition.
func validCIDR(cidr string) bool {
	prefix, err := ipnet.ValidateVPCCIDR(cidr)
	return err == nil && prefix.String() == cidr
}

// mustParsePrefix parses a CIDR that Validate has already accepted. A parse
// failure here is a bug, and returning empty XML would be worse than panicking.
func mustParsePrefix(cidr string) netip.Prefix {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		panic(fmt.Sprintf("networkxml: Validate accepted CIDR %q but ParsePrefix rejects it: %v", cidr, err))
	}
	return prefix
}
