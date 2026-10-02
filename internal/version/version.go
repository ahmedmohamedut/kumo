// Package version carries the build information stamped into the binary with
// -ldflags (see the Makefile) and reported by the API and the CLI.
package version

import "fmt"

// Set at build time; the defaults are used by `go run` and tests.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String renders the build information, e.g. "v0.1.0 (commit 2443345, built 2026-10-02T21:58:00Z)".
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
