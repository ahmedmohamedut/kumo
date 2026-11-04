package dbcred_test

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/service/dbcred"
)

const goodPassword = "correct horse battery staple"

func TestPostgresVerifierShape(t *testing.T) {
	t.Parallel()

	got, err := dbcred.PostgresVerifier(goodPassword)
	if err != nil {
		t.Fatalf("PostgresVerifier(): %v", err)
	}

	// SCRAM-SHA-256$<iter>:<b64 salt>$<b64 StoredKey>:<b64 ServerKey>
	mechanism, rest, ok := strings.Cut(got, "$")
	if !ok || mechanism != "SCRAM-SHA-256" {
		t.Fatalf("verifier = %q, want it to start with SCRAM-SHA-256$", got)
	}
	head, tail, ok := strings.Cut(rest, "$")
	if !ok {
		t.Fatalf("verifier = %q, want a second $", got)
	}

	iter, saltB64, ok := strings.Cut(head, ":")
	if !ok {
		t.Fatalf("verifier = %q, want <iter>:<salt>", got)
	}
	if iter != strconv.Itoa(dbcred.Iterations) {
		t.Errorf("iterations = %q, want %d", iter, dbcred.Iterations)
	}
	salt, err := base64.StdEncoding.DecodeString(saltB64)
	if err != nil {
		t.Fatalf("salt %q is not base64: %v", saltB64, err)
	}
	if len(salt) != dbcred.SaltLength {
		t.Errorf("salt = %d bytes, want %d", len(salt), dbcred.SaltLength)
	}

	storedB64, serverB64, ok := strings.Cut(tail, ":")
	if !ok {
		t.Fatalf("verifier = %q, want <stored>:<server>", got)
	}
	for name, value := range map[string]string{"stored key": storedB64, "server key": serverB64} {
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			t.Fatalf("%s %q is not base64: %v", name, value, err)
		}
		if len(raw) != 32 {
			t.Errorf("%s = %d bytes, want 32", name, len(raw))
		}
	}
}

// The password is the one thing that must not survive. A verifier that
// contained it, in any encoding, would defeat the whole arrangement.
func TestPostgresVerifierDoesNotContainThePassword(t *testing.T) {
	t.Parallel()

	const password = "zaphod-beeblebrox-42"
	got, err := dbcred.PostgresVerifier(password)
	if err != nil {
		t.Fatalf("PostgresVerifier(): %v", err)
	}

	if strings.Contains(got, password) {
		t.Errorf("verifier %q contains the password", got)
	}
	if strings.Contains(got, base64.StdEncoding.EncodeToString([]byte(password))) {
		t.Errorf("verifier %q contains the base64 password", got)
	}
}

// A fresh salt per DB instance is what stops two databases with the same
// password from sharing a verifier.
func TestPostgresVerifierSaltsEachCall(t *testing.T) {
	t.Parallel()

	seen := make(map[string]struct{}, 8)
	for range 8 {
		got, err := dbcred.PostgresVerifier(goodPassword)
		if err != nil {
			t.Fatalf("PostgresVerifier(): %v", err)
		}
		if _, repeat := seen[got]; repeat {
			t.Fatalf("the same verifier came back twice: %q", got)
		}
		seen[got] = struct{}{}
	}
}

func TestPasswordRefusals(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		want     error
	}{
		{"empty", "", dbcred.ErrPasswordTooShort},
		{"one under the floor", strings.Repeat("a", dbcred.MinPasswordLength-1), dbcred.ErrPasswordTooShort},
		{"one over the ceiling", strings.Repeat("a", dbcred.MaxPasswordLength+1), dbcred.ErrPasswordTooLong},
		// SASLprep is the identity only on printable ASCII. A password this
		// package pre-hashed without normalising would be derived differently
		// by the client, and the login would fail with the right password.
		{"a newline", "abcdefghij\n", dbcred.ErrPasswordNotPrintableASCII},
		{"a tab", "abcdefghij\t", dbcred.ErrPasswordNotPrintableASCII},
		{"a NUL", "abcdefghij\x00", dbcred.ErrPasswordNotPrintableASCII},
		{"an accent", "contraseña-segura", dbcred.ErrPasswordNotPrintableASCII},
		{"an emoji", "passphrase-\U0001f511", dbcred.ErrPasswordNotPrintableASCII},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := dbcred.ValidatePassword(tc.password); !errors.Is(err, tc.want) {
				t.Errorf("ValidatePassword() = %v, want %v", err, tc.want)
			}
			if _, err := dbcred.PostgresVerifier(tc.password); !errors.Is(err, tc.want) {
				t.Errorf("PostgresVerifier() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestPasswordAccepts(t *testing.T) {
	t.Parallel()

	for _, password := range []string{
		strings.Repeat("a", dbcred.MinPasswordLength),
		strings.Repeat("a", dbcred.MaxPasswordLength),
		// Every printable ASCII character, including the ones that matter for
		// the shell and SQL quoting of the seed.
		`abcdefghij '"$\{}|;&<>()` + "`" + `!#%^*~[]?/`,
	} {
		if err := dbcred.ValidatePassword(password); err != nil {
			t.Errorf("ValidatePassword(%q) = %v, want nil", password, err)
		}
		if _, err := dbcred.PostgresVerifier(password); err != nil {
			t.Errorf("PostgresVerifier(%q) = %v, want nil", password, err)
		}
	}
}
