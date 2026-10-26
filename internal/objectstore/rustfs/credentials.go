package rustfs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrCredentialsFile means the credentials file is missing, unsafe or
// incomplete. Its messages never quote a value from the file.
var ErrCredentialsFile = errors.New("rustfs: unusable credentials file")

// Credentials are the server's admin keys.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// ReadCredentials reads KUMO_S3_CREDENTIALS_FILE: KEY=value lines, '#'
// comments, optional quotes. It takes access_key/secret_key, or the
// RUSTFS_ACCESS_KEY/RUSTFS_SECRET_KEY names of /etc/default/rustfs so an
// operator can copy that file. It refuses a file group or others can read,
// the way ssh refuses a private key: these keys own every bucket.
func ReadCredentials(path string) (Credentials, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %w", ErrCredentialsFile, err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return Credentials{}, fmt.Errorf("%w: %s has mode %04o; run chmod 600 on it",
			ErrCredentialsFile, path, perm)
	}

	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %w", ErrCredentialsFile, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	var creds Credentials
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return Credentials{}, fmt.Errorf("%w: %s line %d is not KEY=value", ErrCredentialsFile, path, line)
		}
		value = unquote(strings.TrimSpace(value))
		switch strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(key), "export ")) {
		case "access_key", "RUSTFS_ACCESS_KEY":
			creds.AccessKey = value
		case "secret_key", "RUSTFS_SECRET_KEY":
			creds.SecretKey = value
		}
	}
	if err := scanner.Err(); err != nil {
		return Credentials{}, fmt.Errorf("%w: read %s: %w", ErrCredentialsFile, path, err)
	}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return Credentials{}, fmt.Errorf("%w: %s must set access_key and secret_key", ErrCredentialsFile, path)
	}
	return creds, nil
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
