package network

import (
	"net/netip"
	"strings"
	"testing"
)

// sampleRouteTable is what a typical host with one LAN interface and a NATed
// libvirt network reads like: a default route, a connected route for the LAN,
// and a connected route for the Kumo bridge that the scan must ignore.
var sampleRouteTable = []byte(`Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0100A8C0	0003	0	0	0	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
virbr-default	0064A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`)

func TestParseRouteTable(t *testing.T) {
	t.Parallel()

	routes, err := parseRouteTable(sampleRouteTable)
	if err != nil {
		t.Fatalf("parseRouteTable() = %v", err)
	}

	// The default route and the header are dropped; the LAN's connected route
	// and the Kumo bridge's own route are both kept for the scan to filter.
	if len(routes) != 2 {
		t.Fatalf("parseRouteTable() = %d routes, want 2: %+v", len(routes), routes)
	}
	got := routes[0]
	if got.iface != "eth0" {
		t.Errorf("route iface = %q, want eth0", got.iface)
	}
	if want := netip.MustParsePrefix("192.168.0.0/24"); got.prefix != want {
		t.Errorf("route prefix = %s, want %s", got.prefix, want)
	}
	if got := routes[1]; got.iface != "virbr-default" {
		t.Errorf("route iface = %q, want virbr-default", got.iface)
	} else if want := netip.MustParsePrefix("192.168.100.0/24"); got.prefix != want {
		t.Errorf("route prefix = %s, want %s", got.prefix, want)
	}
}

func TestParseRouteTableRejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := parseRouteTable([]byte("eth0 zz 00000000 0001 0 0 0 00FFFFFF 0 0 0")); err == nil {
		t.Error("parseRouteTable() accepted a non-hex destination")
	}
	if _, err := parseRouteTable([]byte("eth0 0000A8C0 00000000 0001 0 0 0 zzFFFFFF 0 0 0")); err == nil {
		t.Error("parseRouteTable() accepted a non-hex mask")
	}
}

func TestFirstHostCollision(t *testing.T) {
	t.Parallel()

	routes := []hostRoute{
		{iface: "eth0", prefix: netip.MustParsePrefix("192.168.0.0/24")},
		{iface: "wlan0", prefix: netip.MustParsePrefix("10.20.0.0/16")},
		// A second VPC's bridge, which the overlap check against vpcs covers.
		{iface: "kmb0a1b2c3d", prefix: netip.MustParsePrefix("172.16.0.0/16")},
	}
	subnets := []hostIface{
		{name: "eth1", prefix: netip.MustParsePrefix("10.0.1.0/24")},
		{name: "virbr-default", prefix: netip.MustParsePrefix("192.168.100.0/24")},
	}

	tests := []struct {
		name   string
		prefix string
		want   string
	}{
		{name: "fresh space", prefix: "10.200.0.0/16", want: ""},
		{name: "under the LAN", prefix: "192.168.0.0/24", want: "the host route 192.168.0.0/24 via eth0"},
		{name: "a supernet of the LAN", prefix: "192.168.0.0/16", want: "the host route 192.168.0.0/24 via eth0"},
		{name: "the wlan net", prefix: "10.20.128.0/17", want: "the host route 10.20.0.0/16 via wlan0"},
		// The Kumo bridges are excluded: an existing VPC row already owns them.
		{name: "a kumo bridge is not a collision", prefix: "172.16.0.0/16", want: ""},
		{name: "a virbr bridge is not a collision", prefix: "192.168.100.0/24", want: ""},
		{name: "an interface without a route", prefix: "10.0.1.0/24", want: "the subnet 10.0.1.0/24 on eth1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefix := netip.MustParsePrefix(tt.prefix)
			got := firstHostCollision(prefix, routes, subnets)
			if got != tt.want {
				t.Errorf("firstHostCollision(%s) = %q, want %q", prefix, got, tt.want)
			}
		})
	}
}

// TestHostCollisionSmoke runs the real host scan and only requires it to answer:
// the environment's routing table is what it is, but reading it and excluding
// Kumo's own bridges must never fail. It also proves the exclusion works, since
// a machine that runs Kumo's networks must be able to create the next VPC.
func TestHostCollisionSmoke(t *testing.T) {
	t.Parallel()

	colliding, err := HostCollision(netip.MustParsePrefix("10.0.0.0/16"))
	if err != nil {
		t.Fatalf("HostCollision() = %v", err)
	}
	if colliding != "" && strings.Contains(colliding, "virbr") {
		t.Errorf("HostCollision() reported %q, a Kumo bridge must be excluded", colliding)
	}
}

func TestLeHexAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		hex  string
		want string
	}{
		{hex: "0000A8C0", want: "192.168.0.0"}, // 192.168.0.0, bytes reversed
		{hex: "0100A8C0", want: "192.168.0.1"},
		{hex: "6400A8C0", want: "192.168.0.100"},
		{hex: "00000000", want: "0.0.0.0"},
		{hex: "FFFFFFFF", want: "255.255.255.255"},
		{hex: "00000001", want: "1.0.0.0"},
	}

	for _, tt := range tests {
		t.Run(tt.hex, func(t *testing.T) {
			t.Parallel()

			addr, err := leHexAddr(tt.hex)
			if err != nil {
				t.Fatalf("leHexAddr(%q) = %v", tt.hex, err)
			}
			if addr.String() != tt.want {
				t.Errorf("leHexAddr(%q) = %s, want %s", tt.hex, addr, tt.want)
			}
		})
	}

	if _, err := leHexAddr("0000"); err == nil {
		t.Error("leHexAddr() accepted a short value")
	}
	if _, err := leHexAddr("zzzzyyyy"); err == nil {
		t.Error("leHexAddr() accepted a non-hex value")
	}
}

func TestMaskBits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr string
		want int
	}{
		{addr: "255.255.0.0", want: 16},
		{addr: "255.255.255.0", want: 24},
		{addr: "255.255.255.240", want: 28},
		{addr: "255.255.0.0", want: 16},
		{addr: "0.0.0.0", want: 0},
		{addr: "255.255.255.255", want: 32},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()

			if got := maskBits(netip.MustParseAddr(tt.addr)); got != tt.want {
				t.Errorf("maskBits(%s) = %d, want %d", tt.addr, got, tt.want)
			}
		})
	}
}
