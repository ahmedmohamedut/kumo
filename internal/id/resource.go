package id

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// ResourceIDSize is the number of lowercase hex characters after the prefix,
// e.g. key-0a1b2c3d4e5f60718 (AGENTS.md, backend conventions). 17 hex chars
// carry 68 bits of randomness, matching the AWS id shape the console mimics.
const ResourceIDSize = 17

// Prefixes of the resource IDs. The instance ID doubles as the libvirt domain
// name (docs/design/02-data-model.md).
const (
	PrefixInstance         = "i"
	PrefixImage            = "img"
	PrefixKeyPair          = "key"
	PrefixTask             = "task"
	PrefixVPC              = "vpc"
	PrefixSubnet           = "subnet"
	PrefixNetworkInterface = "eni"
)

// Resource returns "<prefix>-<17 random hex chars>". It panics only if the
// system random source fails, which cannot be handled here usefully.
func Resource(prefix string) string {
	// 9 bytes hex-encode to 18 characters; one is trimmed.
	var b [9]byte
	_, _ = rand.Read(b[:])

	return prefix + "-" + hex.EncodeToString(b[:])[:ResourceIDSize]
}

// KeyPair returns a key pair ID (key-…).
func KeyPair() string { return Resource(PrefixKeyPair) }

// VPC returns a VPC ID (vpc-…). The id doubles as the libvirt network name; its
// first 8 hex characters seed the bridge interface name (hypervisor.BridgeName).
func VPC() string { return Resource(PrefixVPC) }

// Subnet returns a subnet ID (subnet-…).
func Subnet() string { return Resource(PrefixSubnet) }

// NetworkInterface returns a network interface ID (eni-…).
func NetworkInterface() string { return Resource(PrefixNetworkInterface) }

// ResourcePrefix returns the prefix constant that matches value, or ("", false)
// if no known prefix matches. It iterates the seven Prefix* constants and
// delegates every shape check to HasResourceShape.
func ResourcePrefix(value string) (string, bool) {
	prefixes := [7]string{
		PrefixInstance,
		PrefixImage,
		PrefixKeyPair,
		PrefixTask,
		PrefixVPC,
		PrefixSubnet,
		PrefixNetworkInterface,
	}
	for _, p := range prefixes {
		if HasResourceShape(p, value) {
			return p, true
		}
	}
	return "", false
}

// HasResourceShape reports whether value looks like a resource ID with the
// given prefix. It is a cheap guard for path parameters, not a check that the
// resource exists.
func HasResourceShape(prefix, value string) bool {
	if !strings.HasPrefix(value, prefix+"-") {
		return false
	}
	if len(value) != len(prefix)+1+ResourceIDSize {
		return false
	}
	// DecodeString is no help here: 17 hex chars is an odd length, so the check
	// is per character.
	for _, ch := range value[len(prefix)+1:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}
