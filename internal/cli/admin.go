package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	kumoauth "github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/store"
)

// passwordEnvName carries the password when it cannot be piped in, e.g. from a
// provisioning system. A flag is deliberately absent: argv is world-readable in
// /proc, so a `--password` option would leak the secret to every process on the
// host.
const passwordEnvName = "KUMO_ADMIN_PASSWORD" //nolint:gosec // G101: the name of a variable, not a credential

// minPasswordLength mirrors authservice.MinPasswordLength, repeated so a bad
// password is rejected before the database is opened.
const minPasswordLength = authservice.MinPasswordLength

// runAdmin implements `kumod admin create` and `kumod admin reset-password`.
func runAdmin(ctx context.Context, env Env, args []string) error {
	subcommand, rest, err := splitCommand(args, "admin")
	if err != nil {
		return err
	}

	switch subcommand {
	case "create":
		return adminCreate(ctx, env, rest)
	case "reset-password":
		return adminResetPassword(ctx, env, rest)
	default:
		return fmt.Errorf("%w: unknown admin subcommand %q", ErrUsage, subcommand)
	}
}

// adminCreate adds an administrator to an existing database.
func adminCreate(ctx context.Context, env Env, args []string) error {
	flags, username, fromStdin := adminFlags(env, "kumod admin create",
		"Add an administrator to an existing Kumo database.")
	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *username == "" {
		return fmt.Errorf("%w: --username is required", ErrUsage)
	}

	password, err := readPassword(env, *fromStdin)
	if err != nil {
		return err
	}

	opened, err := openAuth(ctx, env)
	if err != nil {
		return err
	}
	defer opened.close() //nolint:errcheck // nothing to report on a read-mostly command

	user, err := opened.auth.CreateAdmin(ctx, *username, password)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "created administrator %s (%s)\n", user.Username, user.ID)
	return nil
}

// adminResetPassword replaces a user's password and signs their sessions out.
func adminResetPassword(ctx context.Context, env Env, args []string) error {
	flags, username, fromStdin := adminFlags(env, "kumod admin reset-password",
		"Replace the password of an existing user.")
	if err := parseFlags(flags, args); err != nil {
		return err
	}

	if *username == "" {
		return fmt.Errorf("%w: --username is required", ErrUsage)
	}

	password, err := readPassword(env, *fromStdin)
	if err != nil {
		return err
	}

	opened, err := openAuth(ctx, env)
	if err != nil {
		return err
	}
	defer opened.close() //nolint:errcheck // nothing to report on a read-mostly command

	if err := opened.auth.ResetPassword(ctx, *username, password); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "password reset for %s; its sessions were revoked\n", *username)
	return nil
}

// adminFlags returns the flags both admin subcommands take.
func adminFlags(
	env Env,
	name, synopsis string,
) (*flag.FlagSet, *string, *bool) {
	flags := newFlagSet(env, name, synopsis)
	username := flags.String("username", "", "the user to create or update (required)")
	fromStdin := flags.Bool("password-stdin", false,
		"read the password from the first line of standard input")

	return flags, username, fromStdin
}

// readPassword resolves the password from stdin or the environment, then checks
// the policy. It runs before the database is opened so a typo does not need a
// database to be reported.
func readPassword(env Env, fromStdin bool) (string, error) {
	var password string
	if fromStdin {
		line, err := readLine(env.Stdin)
		if err != nil {
			return "", err
		}
		password = line
	} else if fromEnv := os.Getenv(passwordEnvName); fromEnv != "" {
		password = fromEnv
	} else {
		return "", fmt.Errorf("%w: no password given; pass --password-stdin or set %s",
			ErrUsage, passwordEnvName)
	}

	if len(password) < minPasswordLength {
		return "", fmt.Errorf("%w: the password must be at least %d characters",
			service.ErrInvalid, minPasswordLength)
	}

	return password, nil
}

// readLine reads the first line of r and ignores the rest, so
// `kumod admin create --password-stdin < secret.txt` works.
func readLine(r io.Reader) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%w: there is no standard input to read the password from", ErrUsage)
	}

	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the password from stdin: %w", err)
	}

	return strings.TrimRight(line, "\r\n"), nil
}

// authCommand is the database plus the auth service an admin command needs.
type authCommand struct {
	auth  *authservice.Service
	store *store.Store
}

// close releases the database.
func (c *authCommand) close() error { return c.store.Close() }

// openAuth opens the configured database and builds the auth service on it. The
// admin commands never start the API or the worker: they must work against a
// database the server has stopped using.
func openAuth(ctx context.Context, env Env) (*authCommand, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}

	// The admin commands never issue tokens, but the service requires a signer
	// and reading the key file is a useful check in itself: an unreadable or
	// world-readable key would keep the server from starting anyway.
	signer, err := kumoauth.NewSignerFromFile(cfg.JWTSecretFile, cfg.AccessTTL)
	if err != nil {
		db.Close() //nolint:errcheck // the construction error is the one worth reporting
		return nil, err
	}

	authService, err := authservice.New(authservice.Options{
		Logger:     logging.New(cfg.LogLevel, env.Stderr),
		Store:      db,
		Signer:     signer,
		RefreshTTL: cfg.RefreshTTL,
	})
	if err != nil {
		db.Close() //nolint:errcheck // the construction error is the one worth reporting
		return nil, err
	}

	return &authCommand{auth: authService, store: db}, nil
}
