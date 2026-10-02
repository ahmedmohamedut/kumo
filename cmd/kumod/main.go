// Command kumod is the Kumo control plane: HTTP API, task worker and the
// embedded web console. This binary is built by `make build`.
package main

import (
	"fmt"

	"github.com/ahmedmohamedut/kumo/internal/version"
)

func main() {
	fmt.Println("kumo", version.String())
}
