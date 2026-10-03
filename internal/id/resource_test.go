package id_test

import (
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/id"
)

func TestResource(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for range 10_000 {
		got := id.Resource(id.PrefixKeyPair)

		if !id.HasResourceShape(id.PrefixKeyPair, got) {
			t.Fatalf("Resource(%q) = %q, want key-<17 hex>", id.PrefixKeyPair, got)
		}
		if seen[got] {
			t.Fatalf("Resource(%q) returned %q twice", id.PrefixKeyPair, got)
		}
		seen[got] = true
	}
}

func TestKeyPair(t *testing.T) {
	t.Parallel()

	got := id.KeyPair()
	if !strings.HasPrefix(got, "key-") {
		t.Errorf("KeyPair() = %q, want the key- prefix", got)
	}
	if !id.HasResourceShape(id.PrefixKeyPair, got) {
		t.Errorf("KeyPair() = %q fails its own shape check", got)
	}
}

func TestHasResourceShape(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix string
		value  string
		want   bool
	}{
		{name: "a real key pair ID", prefix: id.PrefixKeyPair, value: "key-0a1b2c3d4e5f60718", want: true},
		{name: "wrong prefix", prefix: id.PrefixKeyPair, value: "i-0a1b2c3d4e5f60718", want: false},
		{name: "too short", prefix: id.PrefixKeyPair, value: "key-0a1b2c", want: false},
		{name: "too long", prefix: id.PrefixKeyPair, value: "key-0a1b2c3d4e5f607180", want: false},
		{name: "not hex", prefix: id.PrefixKeyPair, value: "key-zzzzzzzzzzzzzzzzz", want: false},
		{name: "uppercase hex is outside the shape", prefix: id.PrefixKeyPair, value: "key-0A1B2C3D4E5F60718", want: false},
		{name: "empty", prefix: id.PrefixKeyPair, value: "", want: false},
		{name: "prefix only", prefix: id.PrefixKeyPair, value: "key-", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := id.HasResourceShape(tt.prefix, tt.value); got != tt.want {
				t.Errorf("HasResourceShape(%q, %q) = %v, want %v", tt.prefix, tt.value, got, tt.want)
			}
		})
	}
}
