// Command kumod is the Kumo control plane: HTTP API, task worker and the
// embedded web console, plus the operator commands of M1.2. It is built by
// `make build`.
//
// All logic lives in internal/cli; this file only wires the process arguments,
// the interrupt context and the exit code.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ahmedmohamedut/kumo/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run turns one invocation into an exit code.
func run(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return cli.Run(ctx, cli.Env{
		Args:   args,
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
}
