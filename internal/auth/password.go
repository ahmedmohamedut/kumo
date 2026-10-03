// Package auth holds the primitives of the authentication system: password
// hashing, access tokens and refresh tokens. It knows nothing about HTTP or
// SQLite; internal/service/auth builds the flows on top of it.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password parameters. They follow the OWASP recommendation for argon2id
// (64 MiB, three passes); a hash stores them, so raising them later does not
// invalidate existing passwords.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 2
	argonKeyLen  = 32
	saltLen      = 16
)

var (
	// ErrEmptyPassword means HashPassword was called with nothing to hash.
	ErrEmptyPassword = errors.New("auth: the password must not be empty")
	// ErrInvalidPasswordHash means a stored hash is not a PHC string this
	// package can verify against. It never means the password was wrong.
	ErrInvalidPasswordHash = errors.New("auth: invalid password hash")
)

// HashPassword returns an argon2id PHC string for password.
func HashPassword(password string) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}

	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword reports whether password matches the argon2id PHC hash. A
// malformed hash is an error, not a false: callers must not turn a corrupt row
// into a successful login.
func VerifyPassword(hash, password string) (bool, error) {
	params, salt, want, err := decodeHash(hash)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.time, params.memory, params.threads, uint32(len(want)))

	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// hashParams are the cost parameters carried by a PHC string.
type hashParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

// decodeHash parses `$argon2id$v=19$m=…,t=…,p=…$salt$hash`.
func decodeHash(hash string) (hashParams, []byte, []byte, error) {
	var zero hashParams

	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return zero, nil, nil, fmt.Errorf("%w: not an argon2id PHC string", ErrInvalidPasswordHash)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return zero, nil, nil, fmt.Errorf("%w: unsupported version %q", ErrInvalidPasswordHash, parts[2])
	}

	var params hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.memory, &params.time, &params.threads); err != nil {
		return zero, nil, nil, fmt.Errorf("%w: bad cost parameters %q", ErrInvalidPasswordHash, parts[3])
	}
	if params.memory == 0 || params.time == 0 || params.threads == 0 {
		return zero, nil, nil, fmt.Errorf("%w: zero cost parameter in %q", ErrInvalidPasswordHash, parts[3])
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: bad salt: %w", ErrInvalidPasswordHash, err)
	}

	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return zero, nil, nil, fmt.Errorf("%w: bad hash: %w", ErrInvalidPasswordHash, err)
	}

	return params, salt, key, nil
}
