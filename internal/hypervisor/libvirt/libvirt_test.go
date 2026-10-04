package libvirt

import "testing"

func TestHasKVM(t *testing.T) {
	// Just test it doesn't panic
	_ = HasKVM()
}
