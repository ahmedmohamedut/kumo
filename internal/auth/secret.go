package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SecretSize is the length of the HMAC key in bytes. 32 bytes is the smallest
// key HS256 is defined for.
const SecretSize = 32

// SecretFileMode is the mode the signing key is stored with: readable and
// writable by its owner only.
const SecretFileMode os.FileMode = 0o600

// SecretDirMode is the mode of the directory holding the signing key.
const SecretDirMode os.FileMode = 0o700

var (
	// ErrEmptySecretPath means no path was configured for the signing key.
	ErrEmptySecretPath = errors.New("auth: the signing key path must not be empty")
	// ErrSecretIsDirectory means the configured path is a directory.
	ErrSecretIsDirectory = errors.New("auth: the signing key is a directory")
	// ErrSecretNotPrivate means the signing key can be read by other users.
	ErrSecretNotPrivate = errors.New("auth: the signing key is readable by others")
	// ErrSecretTooShort means the signing key holds less than SecretSize bytes.
	ErrSecretTooShort = errors.New("auth: the signing key is too short")
)

// LoadOrCreateSecret returns the signing key stored at path, generating it on
// first run. Losing the file invalidates every issued access token, which is
// why it is kept instead of derived from anything else.
func LoadOrCreateSecret(path string) ([]byte, error) {
	if path == "" {
		return nil, ErrEmptySecretPath
	}

	secret, err := readSecret(path)
	switch {
	case err == nil:
		return secret, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}

	return createSecret(path)
}

// readSecret loads an existing key and refuses one that anyone but the owner
// can read: a world-readable signing key is a signing key in public.
func readSecret(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrSecretIsDirectory, path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("%w: %s is mode %04o, run chmod 600 on it",
			ErrSecretNotPrivate, path, perm)
	}

	secret, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("auth: read signing key: %w", err)
	}

	secret = trimTrailingNewline(secret)
	if len(secret) < SecretSize {
		return nil, fmt.Errorf("%w: %s holds %d bytes, want at least %d",
			ErrSecretTooShort, path, len(secret), SecretSize)
	}

	return secret, nil
}

// createSecret generates a key and writes it with O_EXCL, so two processes
// racing on first start cannot each believe they own the key.
func createSecret(path string) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), SecretDirMode); err != nil {
		return nil, fmt.Errorf("auth: create the signing key directory: %w", err)
	}

	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("auth: generate signing key: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, SecretFileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process won the race; its key is the one that counts.
			return readSecret(path)
		}
		return nil, fmt.Errorf("auth: create signing key: %w", err)
	}

	if _, err := file.Write(secret); err != nil {
		file.Close() //nolint:errcheck // the write error is the one worth reporting
		return nil, fmt.Errorf("auth: write signing key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("auth: close signing key: %w", err)
	}

	return secret, nil
}

// trimTrailingNewline accepts a key written by `openssl rand -hex` or pasted
// into a config file, both of which tend to end in a newline.
func trimTrailingNewline(secret []byte) []byte {
	for len(secret) > 0 && (secret[len(secret)-1] == '\n' || secret[len(secret)-1] == '\r') {
		secret = secret[:len(secret)-1]
	}
	return secret
}

// GenerateSecret returns a fresh signing key, for tests and for the CLI.
func GenerateSecret() ([]byte, error) {
	secret := make([]byte, SecretSize)
	if _, err := io.ReadFull(rand.Reader, secret); err != nil {
		return nil, fmt.Errorf("auth: generate signing key: %w", err)
	}
	return secret, nil
}

// NewSignerFromFile loads the signing key at path, generating it on first run,
// and returns a Signer for it. It is the one place that pairs a key file with a
// lifetime, so the server and the CLI cannot drift apart.
func NewSignerFromFile(path string, access time.Duration) (*Signer, error) {
	secret, err := LoadOrCreateSecret(path)
	if err != nil {
		return nil, err
	}

	signer, err := NewSigner(secret, access)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", err, ErrWeakSecret)
	}

	return signer, nil
}
