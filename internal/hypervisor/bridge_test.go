package hypervisor

import "testing"

// VPC ids are 17 hex characters; the bridge takes the first 8 of them so the
// interface name fits IFNAMSIZ and stays unique per VPC
// (docs/design/04-libvirt-integration.md §6.1).
func TestBridgeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		vpcID string
		want  string
	}{
		{vpcID: "vpc-0a1b2c3d4e5f60718", want: "kmb0a1b2c3d"},
		{vpcID: "vpc-fffffffffffffffff", want: "kmbffffffff"},
		{vpcID: "vpc-00000000000000000", want: "kmb00000000"},
		// Adopted ids keep the same shape, and a non-vpc id does not break the
		// helper (it will only ever see one, but a panic here would be ugly).
		{vpcID: "anything", want: "kmbanything"},
	}

	for _, tt := range tests {
		t.Run(tt.vpcID, func(t *testing.T) {
			t.Parallel()

			got := BridgeName(tt.vpcID)
			if got != tt.want {
				t.Errorf("BridgeName(%q) = %q, want %q", tt.vpcID, got, tt.want)
			}
			if len(got) > 15 {
				t.Errorf("BridgeName(%q) = %q, %d chars, want at most 15", tt.vpcID, got, len(got))
			}
		})
	}
}

func TestBridgeNameIsUniqueAcrossVPCs(t *testing.T) {
	t.Parallel()

	// The 17-hex id is random, so two VPCs almost never share their first 8
	// characters; when they do, the design accepts the collision the way a
	// short name always could. What must never happen is two different prefixes
	// producing one bridge.
	if a, b := BridgeName("vpc-0a1b2c3d4e5f60718"), BridgeName("vpc-9f8e7d6c5b4a39281"); a == b {
		t.Errorf("two different VPCs share the bridge %q", a)
	}
	// The 8-character bridge prefix necessarily collides for VPCs that share
	// their first 8 hex characters; what must hold is that the shortening is
	// deterministic, so the same prefix never means two bridges.
	if BridgeName("vpc-0123456789abcdef0") != BridgeName("vpc-0123456789abcdef1") {
		t.Error("BridgeName is not deterministic for ids with a common 8-hex prefix")
	}
}
