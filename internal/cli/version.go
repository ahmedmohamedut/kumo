package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/ahmedmohamedut/kumo/internal/version"
)

// printVersion implements `kumod version`. The Go runtime line matters when a
// bug report comes from a host with an unexpected toolchain.
func printVersion(out io.Writer) error {
	fmt.Fprintf(out, "kumod %s\n", version.String())
	fmt.Fprintf(out, "  go:      %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	return nil
}
