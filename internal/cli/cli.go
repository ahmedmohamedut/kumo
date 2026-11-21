// Package cli implements the kumod command line: the server itself plus the
// operator commands of docs/design/05-roadmap.md M1.2. cmd/kumod only wires
// this package to the process arguments and the exit code.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/config"
)

// Exit codes. 0 is success, 1 a runtime failure, 2 a malformed invocation.
const (
	ExitOK    = 0
	ExitError = 1
	ExitUsage = 2
)

// ErrUsage marks a malformed command line: the usage text is printed too.
var ErrUsage = errors.New("usage")

// Env is everything the commands need from the outside world, so tests can
// drive them without touching the process environment.
type Env struct {
	Args   []string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run dispatches one kumod invocation and returns its exit code.
func Run(ctx context.Context, env Env) int {
	if env.Stdin == nil {
		env.Stdin = os.Stdin
	}
	if env.Stdout == nil {
		env.Stdout = os.Stdout
	}
	if env.Stderr == nil {
		env.Stderr = os.Stderr
	}

	err := dispatch(ctx, env)
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, flag.ErrHelp):
		return ExitOK
	case errors.Is(err, ErrUsage):
		fmt.Fprintf(env.Stderr, "kumod: %v\n\n", err)
		fmt.Fprint(env.Stderr, usage)
		return ExitUsage
	default:
		fmt.Fprintf(env.Stderr, "kumod: %v\n", err)
		return ExitError
	}
}

// dispatch routes the arguments to a command. No arguments means "run the
// server", which is what `make dev` and the systemd unit do.
func dispatch(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		return serve(ctx, env.Stdout)
	}

	switch command := env.Args[0]; command {
	case "serve":
		return serve(ctx, env.Stdout)
	case "version":
		return printVersion(env.Stdout)
	case "doctor":
		return runDoctor(ctx, env.Stdout)
	case "admin":
		return runAdmin(ctx, env, env.Args[1:])
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	default:
		return fmt.Errorf("%w: unknown command %q", ErrUsage, command)
	}
}

const usage = `kumod is the Kumo control plane.

Usage:
  kumod [serve]                 run the API, worker and console (default)
  kumod admin create            add an administrator
  kumod admin reset-password    replace the password of a user
  kumod doctor                  check the host and data directory
  kumod version                 print the build information

Every command reads its configuration from the KUMO_* environment variables
(docs/design/00-architecture.md §5). doctor and admin also read the ones in
/etc/kumo/kumod.env (KUMO_ENV_FILE, empty for none) that are not already set,
so they see what the service runs with. Run "kumod <command> -h" for the flags of
a command.

Passwords are never read from the command line, because arguments are visible to
every process on the host. Pass them on standard input instead:

  kumod admin create --username ada --password-stdin < secret.txt
  KUMO_ADMIN_PASSWORD=... kumod admin create --username ada
`

// newFlagSet builds a flag set that reports usage errors through ErrUsage
// instead of printing and exiting, so Run owns all the output.
func newFlagSet(env Env, name, synopsis string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(env.Stderr)
	flags.Usage = func() {
		fmt.Fprintf(env.Stderr, "%s\n\n%s", synopsis, usage)
	}
	return flags
}

// parseFlags parses args, turning flag's own errors into usage errors.
func parseFlags(flags *flag.FlagSet, args []string) error {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fmt.Errorf("%w: %w", ErrUsage, err)
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", ErrUsage, flags.Arg(0))
	}
	return nil
}

// splitCommand takes the subcommand of a two-word invocation, e.g. "create" of
// "kumod admin create", and returns it with the arguments after it.
func splitCommand(args []string, command string) (string, []string, error) {
	if len(args) == 0 {
		return "", nil, fmt.Errorf("%w: kumod %s needs a subcommand (create, reset-password)",
			ErrUsage, command)
	}
	if strings.HasPrefix(args[0], "-") {
		return "", nil, fmt.Errorf("%w: kumod %s needs a subcommand before its flags",
			ErrUsage, command)
	}

	return args[0], args[1:], nil
}

// loadEnvFile copies the KUMO_* settings of the env file the systemd unit
// loads into this process, for the operator commands: run by hand they would
// otherwise check the defaults rather than the configuration the service runs
// with. It returns a line for the operator, empty when there is no file.
func loadEnvFile() string {
	path := config.EnvFilePath()
	read, err := config.ApplyEnvFile(path)
	switch {
	case err != nil:
		return fmt.Sprintf("not read, so only this shell's KUMO_* variables count: %v", err)
	case read:
		return path + " (variables already set win)"
	default:
		return ""
	}
}
