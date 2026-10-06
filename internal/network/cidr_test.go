package network

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestValidateVPCCIDR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cidr    string
		wantOK  bool
		wantErr string
	}{
		{name: "10/16", cidr: "10.0.0.0/16", wantOK: true},
		{name: "172.16/12 border", cidr: "172.16.0.0/16", wantOK: true},
		{name: "192.168/24", cidr: "192.168.1.0/24", wantOK: true},
		{name: "thinnest /28", cidr: "10.0.0.0/28", wantOK: true},
		{name: "widest /16", cidr: "10.0.0.0/16", wantOK: true},
		{name: "private range mid", cidr: "172.31.0.0/16", wantOK: true},

		{name: "not a CIDR", cidr: "banana"},
		{name: "IPv6", cidr: "2001:db8::/32", wantErr: "not IPv4"},
		{name: "public address", cidr: "8.8.8.0/24", wantErr: "not RFC 1918"},
		{name: "link-local", cidr: "169.254.0.0/16", wantErr: "not RFC 1918"},
		{name: "too wide /8", cidr: "10.0.0.0/8", wantErr: "too wide"},
		{name: "too narrow /29", cidr: "10.0.0.0/29", wantErr: "too narrow"},
		{name: "not a network address", cidr: "10.0.1.5/24", wantErr: "does not name a network"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefix, err := ValidateVPCCIDR(tt.cidr)
			switch {
			case tt.wantOK && err != nil:
				t.Fatalf("ValidateVPCCIDR(%q) = %v, want nil", tt.cidr, err)
			case tt.wantOK && prefix.String() != tt.cidr:
				t.Errorf("ValidateVPCCIDR(%q) = %s, want %s", tt.cidr, prefix, tt.cidr)
			case !tt.wantOK && err == nil:
				t.Fatalf("ValidateVPCCIDR(%q) = nil, want an error", tt.cidr)
			case !tt.wantOK && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("ValidateVPCCIDR(%q) = %v, want it to say %q", tt.cidr, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSubnetCIDR(t *testing.T) {
	t.Parallel()

	vpc := netip.MustParsePrefix("10.0.0.0/16")

	tests := []struct {
		name    string
		cidr    string
		wantOK  bool
		wantErr string
	}{
		{name: "whole VPC", cidr: "10.0.0.0/16", wantErr: "whole"},
		{name: "contained /24", cidr: "10.0.1.0/24", wantOK: true},
		{name: "contained /28", cidr: "10.0.255.240/28", wantOK: true},
		{name: "outside the VPC", cidr: "10.1.0.0/24", wantErr: "not fully contained"},
		{name: "a different private block", cidr: "192.168.1.0/24", wantErr: "not fully contained"},
		// A /28 cannot straddle a /16 border: aligned subnets of a parent are
		// either wholly inside it or wholly outside, and 10.0.255.240/28 ends at
		// 10.0.255.255 exactly. The one over the border starts outside it.
		{name: "the border's far side", cidr: "10.1.0.0/16", wantErr: "not fully contained"},
		{name: "wider than any allowed subnet", cidr: "10.0.0.0/8", wantErr: "too wide"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefix, err := ValidateSubnetCIDR(tt.cidr, vpc)
			switch {
			case tt.wantOK && err != nil:
				t.Fatalf("ValidateSubnetCIDR(%q) = %v, want nil", tt.cidr, err)
			case tt.wantOK && prefix.String() != tt.cidr:
				t.Errorf("ValidateSubnetCIDR(%q) = %s, want %s", tt.cidr, prefix, tt.cidr)
			case !tt.wantOK && err == nil:
				t.Fatalf("ValidateSubnetCIDR(%q) = nil, want an error", tt.cidr)
			case !tt.wantOK && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("ValidateSubnetCIDR(%q) = %v, want it to say %q", tt.cidr, err, tt.wantErr)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{name: "siblings side by side", a: "10.0.0.0/24", b: "10.0.1.0/24", want: false},
		{name: "siblings share a border", a: "10.0.0.0/25", b: "10.0.0.128/25", want: false},
		{name: "one swallows the other", a: "10.0.0.0/16", b: "10.0.1.0/24", want: true},
		{name: "partial overlap", a: "10.0.0.0/24", b: "10.0.0.128/25", want: true},
		{name: "same range", a: "10.0.0.0/24", b: "10.0.0.0/24", want: true},
		{name: "different VPC space", a: "10.0.0.0/24", b: "192.168.0.0/24", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			a, err := ValidateVPCCIDR(tt.a)
			if err != nil {
				t.Fatalf("ValidateVPCCIDR(%q) = %v", tt.a, err)
			}
			b, err := ValidateVPCCIDR(tt.b)
			if err != nil {
				t.Fatalf("ValidateVPCCIDR(%q) = %v", tt.b, err)
			}
			if got := Overlaps(a, b); got != tt.want {
				t.Errorf("Overlaps(%s, %s) = %v, want %v", a, b, got, tt.want)
			}
		})
	}
}

func TestGatewayAndNetmask(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cidr    string
		gateway string
		netmask string
	}{
		{cidr: "10.0.0.0/16", gateway: "10.0.0.1", netmask: "255.255.0.0"},
		{cidr: "172.16.0.0/16", gateway: "172.16.0.1", netmask: "255.255.0.0"},
		{cidr: "192.168.0.0/24", gateway: "192.168.0.1", netmask: "255.255.255.0"},
		{cidr: "10.0.0.0/28", gateway: "10.0.0.1", netmask: "255.255.255.240"},
		{cidr: "10.0.0.128/25", gateway: "10.0.0.129", netmask: "255.255.255.128"},
	}

	for _, tt := range tests {
		t.Run(tt.cidr, func(t *testing.T) {
			t.Parallel()

			prefix, err := ValidateVPCCIDR(tt.cidr)
			if err != nil {
				t.Fatalf("ValidateVPCCIDR(%q) = %v", tt.cidr, err)
			}
			if got := Gateway(prefix).String(); got != tt.gateway {
				t.Errorf("Gateway(%s) = %s, want %s", prefix, got, tt.gateway)
			}
			if got := Netmask(prefix); got != tt.netmask {
				t.Errorf("Netmask(%s) = %s, want %s", prefix, got, tt.netmask)
			}
		})
	}
}

func TestUsableCount(t *testing.T) {
	t.Parallel()

	vpc := netip.MustParsePrefix("10.0.0.0/16")
	gateway := Gateway(vpc)

	tests := []struct {
		name string
		sub  string
		want int64
	}{
		// 254 usable addresses in a /24, none of them the VPC gateway.
		{name: "sibling /24", sub: "10.0.1.0/24", want: 254},
		// The VPC gateway 10.0.0.1 sits in this /24, so one more is reserved.
		{name: "the gateway's /24", sub: "10.0.0.0/24", want: 253},
		// A /28 holds 16 addresses; 14 remain after network and broadcast.
		{name: "a /28 without the gateway", sub: "10.0.1.0/28", want: 14},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefix, err := ValidateSubnetCIDR(tt.sub, vpc)
			if err != nil {
				t.Fatalf("ValidateSubnetCIDR(%q) = %v", tt.sub, err)
			}
			if got := UsableCount(prefix, gateway); got != tt.want {
				t.Errorf("UsableCount(%s) = %d, want %d", prefix, got, tt.want)
			}
		})
	}
}

func TestForEachUsable(t *testing.T) {
	t.Parallel()

	// The VPC gateway 10.0.0.1 falls inside this /28, so the walk must skip it
	// and start at 10.0.0.2, ending at 10.0.0.14.
	prefix, err := ValidateSubnetCIDR("10.0.0.0/28", netip.MustParsePrefix("10.0.0.0/16"))
	if err != nil {
		t.Fatalf("ValidateSubnetCIDR() = %v", err)
	}
	gateway := netip.MustParseAddr("10.0.0.1")

	var got []string
	ForEachUsable(prefix, gateway, func(addr netip.Addr) bool {
		got = append(got, addr.String())
		return true
	})

	var want []string
	for i := 2; i <= 14; i++ {
		want = append(want, fmt.Sprintf("10.0.0.%d", i))
	}
	if len(got) != len(want) {
		t.Fatalf("ForEachUsable(%s) visited %v, want %v", prefix, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ForEachUsable(%s)[%d] = %s, want %s", prefix, i, got[i], want[i])
		}
	}

	// A visitor that stops early must stop the walk.
	stops := 0
	ForEachUsable(prefix, gateway, func(netip.Addr) bool {
		stops++
		return stops < 2
	})
	if stops != 2 {
		t.Errorf("ForEachUsable() called the visitor %d times, want 2", stops)
	}
}
