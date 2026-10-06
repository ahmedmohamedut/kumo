package libvirt

import "testing"

// Bridge interface names are limited to IFNAMSIZ (15 characters); asking libvirt
// for a longer one fails a network start with "Numerical result out of range".
// bridgeName has to produce something that fits while staying unique per network.
func TestBridgeNameFitsIFNAMSIZ(t *testing.T) {
	t.Parallel()

	// Long names must stay unique per network even when they share a prefix.
	if a, b := bridgeName("kumo-it-launch-net"), bridgeName("kumo-my-long-network-name"); a == b {
		t.Errorf("two long network names share the bridge %q", a)
	}

	tests := []struct {
		network string
		want    string
	}{
		{network: "kumo-default", want: "virbr-default"},
		{network: "kumo-net", want: "virbr-net"},
		{network: "kumo-it-launch-net"},
		{network: "kumo-my-long-network-name"},
	}

	for _, tt := range tests {
		t.Run(tt.network, func(t *testing.T) {
			t.Parallel()

			got := bridgeName(tt.network)
			if len(got) > 15 {
				t.Errorf("bridgeName(%q) = %q, %d chars, want at most 15", tt.network, got, len(got))
			}
			if tt.want != "" && got != tt.want {
				t.Errorf("bridgeName(%q) = %q, want %q", tt.network, got, tt.want)
			}
		})
	}
}
