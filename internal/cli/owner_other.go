//go:build !unix

package cli

// ownerUID has no meaning without unix ownership; doctor then gives no
// account hint rather than guessing.
func ownerUID(string) (int, bool) { return 0, false }
