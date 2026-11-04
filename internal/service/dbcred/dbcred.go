// Package dbcred turns an admin password into something safe to put on a
// cloud-init seed.
//
// A DB instance's password has to reach a Postgres role inside a guest Kumo
// cannot talk to, and the only channel is the seed, which is a file in the
// instances pool (docs/design/07-databases.md section 5). So the password does
// not travel: Postgres accepts a pre-computed SCRAM-SHA-256 verifier wherever
// it accepts a password, and the verifier is what the seed carries.
//
// A verifier is a password hash. It is an offline guessing target at 4096
// PBKDF2 iterations and enough to impersonate the server to a client, but it is
// not the password, and nothing in Kumo keeps the password after this package
// has run.
//
// The package is deliberately free of the database, the hypervisor and the
// services, so every rule here is a function with a test.
package dbcred

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Iterations is the PBKDF2 work factor written into the verifier. It is
// Postgres's own default for password_encryption = scram-sha-256, and it is
// recorded in the verifier string, so a client derives with the same count
// without being told.
const Iterations = 4096

// SaltLength is the salt size in bytes, fresh per DB instance so two databases
// with the same password do not share a verifier.
const SaltLength = 16

// MinPasswordLength and MaxPasswordLength bound an admin password. The floor is
// the one a Kumo user's own password has (docs/design/01-api.md, Auth); the
// ceiling keeps a single field from carrying a megabyte into a seed.
const (
	MinPasswordLength = 10
	MaxPasswordLength = 128
)

// Errors a caller maps onto field errors. They are sentinels rather than
// messages so the service layer can decide the wording.
var (
	// ErrPasswordTooShort means the password is under MinPasswordLength.
	ErrPasswordTooShort = errors.New("dbcred: the password is too short")
	// ErrPasswordTooLong means the password is over MaxPasswordLength.
	ErrPasswordTooLong = errors.New("dbcred: the password is too long")
	// ErrPasswordNotPrintableASCII means the password contains a byte this
	// package will not pre-hash. See PostgresVerifier on SASLprep.
	ErrPasswordNotPrintableASCII = errors.New("dbcred: the password must be printable ASCII")
)

// PostgresVerifier derives the SCRAM-SHA-256 verifier for password, with a
// fresh random salt.
//
// The result goes straight into `CREATE ROLE ... PASSWORD '<verifier>'`, which
// Postgres stores verbatim instead of hashing again.
//
// Only printable ASCII is accepted, and that is a correctness rule rather than
// a style one. SCRAM runs the password through SASLprep (RFC 4013) before
// deriving, on the client and inside Postgres alike. This package does not
// implement SASLprep, which is safe exactly while the password is printable
// ASCII, where SASLprep is the identity. For anything else the client would
// derive from a normalised password, this would have derived from the raw one,
// and the login would fail with a correct password -- the worst way for this to
// go wrong, because nothing would look broken until a user tried to connect.
func PostgresVerifier(password string) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}

	salt := make([]byte, SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("dbcred: read a salt: %w", err)
	}
	return verifierWithSalt(password, salt)
}

// ValidatePassword reports whether a password can be turned into a verifier,
// so a caller can refuse a create request before anything else happens.
func ValidatePassword(password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return ErrPasswordTooShort
	case len(password) > MaxPasswordLength:
		return ErrPasswordTooLong
	}
	for i := 0; i < len(password); i++ {
		// Space through tilde: the printable ASCII range, where SASLprep
		// changes nothing. Tab, newline and anything multi-byte are refused.
		if password[i] < 0x20 || password[i] > 0x7e {
			return ErrPasswordNotPrintableASCII
		}
	}
	return nil
}

// verifierWithSalt is the derivation itself, with the salt supplied so the
// known-answer test can pin the exact string a real Postgres accepted.
func verifierWithSalt(password string, salt []byte) (string, error) {
	// The salted password is PBKDF2-HMAC-SHA256 over the password and salt at
	// Iterations rounds, 32 bytes out.
	salted, err := pbkdf2.Key(sha256.New, password, salt, Iterations, sha256.Size)
	if err != nil {
		return "", fmt.Errorf("dbcred: derive the salted password: %w", err)
	}

	// The stored key is the SHA-256 of the client key, and the client key is
	// an HMAC of the salted password under the literal "Client Key". It is
	// what the server checks a client's proof against.
	stored := sha256.Sum256(mac(salted, "Client Key"))
	// The server key is the matching HMAC under "Server Key", and is how the
	// server proves itself to the client. Postgres keeps both in one string.
	server := mac(salted, "Server Key")

	b64 := base64.StdEncoding.EncodeToString
	var out strings.Builder
	fmt.Fprintf(&out, "SCRAM-SHA-256$%d:%s$%s:%s",
		Iterations, b64(salt), b64(stored[:]), b64(server))
	return out.String(), nil
}

func mac(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	return h.Sum(nil)
}
