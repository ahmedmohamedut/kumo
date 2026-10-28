package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/cli"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/store"
)

const cliPassword = "correct horse battery staple"

// runCLI invokes the command line with a scratch data directory and returns the
// exit code plus both streams.
func runCLI(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), cli.Env{
		Args:   args,
		Stdin:  strings.NewReader(stdin),
		Stdout: &stdout,
		Stderr: &stderr,
	})

	return code, stdout.String(), stderr.String()
}

// libvirtReachable probes the daemon `kumod doctor` would check, and reports
// whether one answers. The URI it tries is the default, which is what the
// doctor test runs with: it never sets KUMO_LIBVIRT_URI.
func libvirtReachable(t *testing.T) bool {
	t.Helper()

	client, err := libvirt.New(logging.Discard(), config.DefaultLibvirtURI)
	if err != nil {
		return false
	}
	defer client.Close() //nolint:errcheck // a read-only probe

	if err := client.Connect(t.Context()); err != nil {
		return false
	}
	return client.Ping(t.Context()) == nil
}

// testDir points KUMO_* at a scratch directory. It is not parallel-safe because
// t.Setenv is not, which is why the CLI tests run sequentially.
func testDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("KUMO_DATA_DIR", dir)
	t.Setenv("KUMO_HYPERVISOR", "fake")
	t.Setenv("KUMO_LOG_LEVEL", "error")
	t.Setenv("KUMO_COOKIE_SECURE", "false")
	t.Setenv("KUMO_ADMIN_PASSWORD", "")

	return dir
}

// openDB opens the database the CLI wrote to, with the schema applied.
func openDB(t *testing.T, dir string) *store.Store {
	t.Helper()

	db, err := store.Open(t.Context(), filepath.Join(dir, "kumo.db"))
	if err != nil {
		t.Fatalf("store.Open(): %v", err)
	}
	t.Cleanup(func() { db.Close() }) //nolint:errcheck // closing cannot fail here

	return db
}

// serviceFor builds an auth service on the CLI's database, so a test can prove
// a command really changed what the API would see.
func serviceFor(t *testing.T, db *store.Store) *authservice.Service {
	t.Helper()

	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}

	svc, err := authservice.New(authservice.Options{
		Logger:     logging.Discard(),
		Store:      db,
		Signer:     signer,
		RefreshTTL: 168 * time.Hour,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}

	return svc
}

func TestVersion(t *testing.T) {
	code, stdout, stderr := runCLI(t, "", "version")

	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "kumod ") {
		t.Errorf("stdout = %q, want the build information", stdout)
	}
	if !strings.Contains(stdout, "go:") {
		t.Errorf("stdout = %q, want the Go runtime line", stdout)
	}
}

func TestHelp(t *testing.T) {
	code, stdout, _ := runCLI(t, "", "help")

	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	for _, command := range []string{"admin create", "admin reset-password", "doctor", "version"} {
		if !strings.Contains(stdout, command) {
			t.Errorf("stdout = %q, want it to mention %q", stdout, command)
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, stderr := runCLI(t, "", "frobnicate")

	if code != cli.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, cli.ExitUsage)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr = %q, want it to name the unknown command", stderr)
	}
	if !strings.Contains(stderr, "Usage:") {
		t.Errorf("stderr = %q, want the usage text", stderr)
	}
}

func TestAdminCreate(t *testing.T) {
	dir := testDir(t)

	code, stdout, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ada", "--password-stdin")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created administrator ada") {
		t.Errorf("stdout = %q, want it to confirm the account", stdout)
	}

	db := openDB(t, dir)
	users, err := db.CountUsers(t.Context())
	if err != nil {
		t.Fatalf("CountUsers(): %v", err)
	}
	if users != 1 {
		t.Errorf("users = %d, want 1", users)
	}

	// The account the CLI created can log in through the API path.
	session, err := serviceFor(t, db).Login(t.Context(), "ada", cliPassword, "", "")
	if err != nil {
		t.Fatalf("Login() error = %v, want the created account to work", err)
	}
	if session.User.Role != auth.RoleAdmin {
		t.Errorf("role = %q, want admin", session.User.Role)
	}
}

func TestAdminCreateReadsTheEnvironment(t *testing.T) {
	dir := testDir(t)
	t.Setenv("KUMO_ADMIN_PASSWORD", cliPassword)

	code, stdout, stderr := runCLI(t, "", "admin", "create", "--username", "ada")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "created administrator ada") {
		t.Errorf("stdout = %q, want it to confirm the account", stdout)
	}

	if _, err := serviceFor(t, openDB(t, dir)).Login(t.Context(), "ada", cliPassword, "", ""); err != nil {
		t.Errorf("Login() error = %v", err)
	}
}

func TestAdminCreateRejectsATakenUsername(t *testing.T) {
	testDir(t)

	if code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("the first create failed: exit %d (stderr %s)", code, stderr)
	}

	code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ADA", "--password-stdin")
	if code != cli.ExitError {
		t.Fatalf("exit code = %d, want 1 for a taken username", code)
	}
	if !strings.Contains(stderr, "taken") {
		t.Errorf("stderr = %q, want it to say the username is taken", stderr)
	}
}

// TestAdminResetPassword is the acceptance criterion of M1.2: the command
// changes the password of a user in an existing database.
func TestAdminResetPassword(t *testing.T) {
	dir := testDir(t)

	if code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("create failed: exit %d (stderr %s)", code, stderr)
	}

	const newPassword = "a brand new passphrase"

	code, stdout, stderr := runCLI(t, newPassword+"\n",
		"admin", "reset-password", "--username", "ada", "--password-stdin")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr %s)", code, stderr)
	}
	if !strings.Contains(stdout, "password reset for ada") {
		t.Errorf("stdout = %q, want it to confirm the reset", stdout)
	}

	svc := serviceFor(t, openDB(t, dir))

	if _, err := svc.Login(t.Context(), "ada", cliPassword, "", ""); !errors.Is(err, errUnauthorized) {
		t.Errorf("Login() with the old password: error = %v, want it to be rejected", err)
	}
	if _, err := svc.Login(t.Context(), "ada", newPassword, "", ""); err != nil {
		t.Errorf("Login() with the new password: error = %v, want it to work", err)
	}
}

func TestAdminResetPasswordRevokesSessions(t *testing.T) {
	dir := testDir(t)

	if code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("create failed: exit %d (stderr %s)", code, stderr)
	}

	svc := serviceFor(t, openDB(t, dir))
	session, err := svc.Login(t.Context(), "ada", cliPassword, "", "cli")
	if err != nil {
		t.Fatalf("Login(): %v", err)
	}

	if code, _, stderr := runCLI(t, "another good passphrase\n",
		"admin", "reset-password", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("reset failed: exit %d (stderr %s)", code, stderr)
	}

	// A password reset ends the sessions it may have been asked to clean up
	// after: the refresh token of the old password must not work any more.
	if _, err := svc.Refresh(t.Context(), session.RefreshToken, ""); !errors.Is(err, errUnauthorized) {
		t.Errorf("Refresh() after a reset: error = %v, want it to be rejected", err)
	}
}

func TestAdminResetPasswordRejectsAnUnknownUser(t *testing.T) {
	testDir(t)

	code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "reset-password", "--username", "nobody", "--password-stdin")
	if code != cli.ExitError {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no user named") {
		t.Errorf("stderr = %q, want it to name the missing user", stderr)
	}
}

func TestAdminCommandsRefuseWeakInput(t *testing.T) {
	dir := testDir(t)

	tests := []struct {
		name  string
		stdin string
		args  []string
		want  int
		says  string
	}{
		{
			name:  "no username",
			stdin: cliPassword + "\n",
			args:  []string{"admin", "create", "--password-stdin"},
			want:  cli.ExitUsage,
			says:  "--username is required",
		},
		{
			name: "no password source",
			args: []string{"admin", "create", "--username", "ada"},
			want: cli.ExitUsage,
			says: "no password given",
		},
		{
			name:  "a short password on stdin",
			stdin: "short\n",
			args:  []string{"admin", "create", "--username", "ada", "--password-stdin"},
			want:  cli.ExitError,
			says:  "at least 10 characters",
		},
		{
			name: "an unknown subcommand",
			args: []string{"admin", "promote"},
			want: cli.ExitUsage,
			says: `unknown admin subcommand "promote"`,
		},
		{
			name: "a missing subcommand",
			args: []string{"admin"},
			want: cli.ExitUsage,
			says: "needs a subcommand",
		},
		{
			name: "an unknown flag",
			args: []string{"admin", "create", "--username", "ada", "--sudo"},
			want: cli.ExitUsage,
			says: "flag provided but not defined",
		},
		{
			name:  "a stray argument",
			stdin: cliPassword + "\n",
			args:  []string{"admin", "create", "--username", "ada", "--password-stdin", "extra"},
			want:  cli.ExitUsage,
			says:  `unexpected argument "extra"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := runCLI(t, tt.stdin, tt.args...)
			if code != tt.want {
				t.Fatalf("exit code = %d, want %d (stderr %s)", code, tt.want, stderr)
			}
			if !strings.Contains(stderr, tt.says) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.says)
			}
		})
	}

	// None of the rejected invocations may have touched the database.
	if _, err := os.Stat(filepath.Join(dir, "kumo.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the database exists after only failed commands: %v", err)
	}
}

func TestReadPasswordFromStdinStopsAtTheFirstLine(t *testing.T) {
	dir := testDir(t)

	// A file with a trailing note, or a shell pipe with more than one line,
	// must not become part of the password.
	stdin := cliPassword + "\nignored second line\n"

	if code, _, stderr := runCLI(t, stdin,
		"admin", "create", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 (stderr %s)", code, stderr)
	}

	if _, err := serviceFor(t, openDB(t, dir)).Login(t.Context(), "ada", cliPassword, "", ""); err != nil {
		t.Errorf("Login() error = %v, want the first line to be the password", err)
	}
}

func TestDoctor(t *testing.T) {
	dir := testDir(t)

	code, stdout, stderr := runCLI(t, "", "doctor")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0 on the fake backend (stderr %s)\n%s", code, stderr, stdout)
	}

	for _, want := range []string{"configuration", "data directory", "disk space", "database",
		"jwt signing key", "hypervisor", "databases"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want a line for %q", stdout, want)
		}
	}
	// The doctor created the signing key, so the server can start right after.
	info, err := os.Stat(filepath.Join(dir, "jwt.key"))
	if err != nil {
		t.Fatalf("the doctor did not create the signing key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the signing key has mode %04o, want 0600", perm)
	}
}

func TestDoctorReportsAnUnfinishedSetup(t *testing.T) {
	testDir(t)

	code, stdout, _ := runCLI(t, "", "doctor")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "setup is open") {
		t.Errorf("stdout = %q, want it to report the open setup", stdout)
	}

	if code, _, stderr := runCLI(t, cliPassword+"\n",
		"admin", "create", "--username", "ada", "--password-stdin"); code != cli.ExitOK {
		t.Fatalf("create failed: exit %d (stderr %s)", code, stderr)
	}

	_, stdout, _ = runCLI(t, "", "doctor")
	if !strings.Contains(stdout, "1 user(s) exist") {
		t.Errorf("stdout = %q, want it to count the user", stdout)
	}
}

// The databases module is on by default and needs no outside server, so the
// only thing doctor can report about it is whether a golden image exists. A
// node with none warns rather than fails: the module is on because nobody
// turned it off, and nothing is broken until somebody tries to create a DB
// instance (docs/design/07-databases.md section 2).
func TestDoctorReportsTheDatabasesModule(t *testing.T) {
	testDir(t)

	code, stdout, _ := runCLI(t, "", "doctor")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0: a missing engine image is a warning, not a failure", code)
	}
	if !strings.Contains(stdout, "no engine image is available") {
		t.Errorf("stdout = %q, want the missing engine image reported", stdout)
	}
	if !strings.Contains(stdout, "warn  databases") {
		t.Errorf("stdout = %q, want the databases line to warn", stdout)
	}
}

func TestDoctorReportsTheDatabasesModuleOff(t *testing.T) {
	testDir(t)
	t.Setenv("KUMO_DATABASES", "off")

	code, stdout, _ := runCLI(t, "", "doctor")
	if code != cli.ExitOK {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "ok    databases          off") {
		t.Errorf("stdout = %q, want the databases line to report it off", stdout)
	}
}

func TestDoctorRunsTheRealLibvirtChecks(t *testing.T) {
	// On a host with a daemon this test would be wrong twice over: doctor's
	// libvirt lines would pass rather than fail, and EnsureInfra would define
	// the production pools (kumo-images, kumo-instances) against the test's
	// scratch data dir. That is a real install being repointed at a directory
	// that vanishes when the test run ends, so the test must not run there.
	if libvirtReachable(t) {
		t.Skip("a real libvirt daemon is reachable; this test only covers the failure report")
	}

	testDir(t)
	t.Setenv("KUMO_HYPERVISOR", "libvirt")

	// The socket almost certainly does not exist in the test environment, so
	// the libvirt lines must fail with the reason, not be reported as pending.
	code, stdout, _ := runCLI(t, "", "doctor")
	if code != cli.ExitError {
		t.Fatalf("exit code = %d, want 1 (stdout %s)", code, stdout)
	}
	for _, want := range []string{"/dev/kvm", "libvirt socket"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want a line for %q", stdout, want)
		}
	}
	if !strings.Contains(stdout, "FAIL") {
		t.Errorf("stdout = %q, want a failing libvirt line", stdout)
	}
	if strings.Contains(stdout, "M3.1") || strings.Contains(stdout, "skip") {
		t.Errorf("stdout = %q, want no pending libvirt checks left", stdout)
	}
}

func TestDoctorFailsOnAMissingDataDirectory(t *testing.T) {
	t.Setenv("KUMO_DATA_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("KUMO_HYPERVISOR", "fake")

	code, stdout, _ := runCLI(t, "", "doctor")
	if code != cli.ExitError {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout, "FAIL  data directory") {
		t.Errorf("stdout = %q, want a failing data directory line", stdout)
	}
}

func TestDoctorObjectStore(t *testing.T) {
	t.Run("off", func(t *testing.T) {
		testDir(t)
		_, stdout, _ := runCLI(t, "", "doctor")
		if !strings.Contains(stdout, "ok    object store") || !strings.Contains(stdout, "off") {
			t.Errorf("stdout = %q, want a passing line saying the module is off", stdout)
		}
	})

	t.Run("unsafe credentials file", func(t *testing.T) {
		dir := testDir(t)
		path := filepath.Join(dir, "s3.credentials")
		if err := os.WriteFile(path, []byte("access_key=a\nsecret_key=topsecret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUMO_OBJECTSTORE", "rustfs")
		t.Setenv("KUMO_S3_ENDPOINT", "http://127.0.0.1:1")

		code, stdout, _ := runCLI(t, "", "doctor")
		if code != cli.ExitError || !strings.Contains(stdout, "FAIL  object store") || !strings.Contains(stdout, "chmod 600") {
			t.Errorf("exit %d, stdout = %q, want a failing line asking for chmod 600", code, stdout)
		}
		if strings.Contains(stdout, "topsecret") {
			t.Errorf("stdout quotes the secret: %q", stdout)
		}
	})

	t.Run("unreachable server", func(t *testing.T) {
		dir := testDir(t)
		if err := os.WriteFile(filepath.Join(dir, "s3.credentials"), []byte("access_key=a\nsecret_key=b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("KUMO_OBJECTSTORE", "rustfs")
		t.Setenv("KUMO_S3_ENDPOINT", "http://127.0.0.1:1")

		code, stdout, _ := runCLI(t, "", "doctor")
		if code != cli.ExitError || !strings.Contains(stdout, "FAIL  object store") {
			t.Errorf("exit %d, stdout = %q, want a failing object store line", code, stdout)
		}
	})
}

func TestDoctorRejectsABadConfiguration(t *testing.T) {
	t.Setenv("KUMO_WORKERS", "0")

	code, _, stderr := runCLI(t, "", "doctor")
	if code != cli.ExitError {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "configuration is not usable") {
		t.Errorf("stderr = %q, want a configuration error", stderr)
	}
}

// errUnauthorized is what the service answers a wrong password with.
var errUnauthorized = service.ErrUnauthorized
