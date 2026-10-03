//go:build !linux

package cli

// freeBytes has no portable implementation outside linux; the doctor reports the
// space as "not measurable" there rather than guessing.
func freeBytes(string) uint64 { return 0 }
