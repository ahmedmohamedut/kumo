package auth_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/auth"
)

func TestLoadOrCreateSecretGeneratesOnFirstRun(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "jwt.key")

	secret, err := auth.LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() error = %v", err)
	}
	if len(secret) != auth.SecretSize {
		t.Errorf("secret is %d bytes, want %d", len(secret), auth.SecretSize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the generated key: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}

	// A second run reads the same key back, or every token would break.
	again, err := auth.LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() error = %v", err)
	}
	if string(again) != string(secret) {
		t.Error("LoadOrCreateSecret() generated a second key instead of reading the first")
	}
}

func TestLoadOrCreateSecretCreatesTheDirectory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "deeper", "jwt.key")

	if _, err := auth.LoadOrCreateSecret(path); err != nil {
		t.Fatalf("LoadOrCreateSecret() error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the key was not created: %v", err)
	}
}

func TestLoadOrCreateSecretRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{
			name:  "an empty path",
			setup: func(*testing.T, string) {},
		},
		{
			name: "a short key",
			setup: func(t *testing.T, path string) {
				writeFile(t, path, "too short", 0o600)
			},
		},
		{
			name: "a world readable key",
			setup: func(t *testing.T, path string) {
				writeFile(t, path, strings.Repeat("k", 32), 0o644)
			},
		},
		{
			name: "a group readable key",
			setup: func(t *testing.T, path string) {
				writeFile(t, path, strings.Repeat("k", 32), 0o640)
			},
		},
		{
			name: "a directory in place of the key",
			setup: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := ""
			if tt.name != "an empty path" {
				path = filepath.Join(t.TempDir(), "jwt.key")
				tt.setup(t, path)
			}

			if _, err := auth.LoadOrCreateSecret(path); err == nil {
				t.Error("LoadOrCreateSecret() error = nil, want an error")
			}
		})
	}
}

func TestLoadOrCreateSecretAcceptsATrailingNewline(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "jwt.key")
	want := strings.Repeat("k", auth.SecretSize)
	writeFile(t, path, want+"\n", 0o600)

	secret, err := auth.LoadOrCreateSecret(path)
	if err != nil {
		t.Fatalf("LoadOrCreateSecret() error = %v", err)
	}
	if string(secret) != want {
		t.Errorf("secret = %q, want the key without its newline", secret)
	}
}

func TestLoadOrCreateSecretKeepsAGeneratedKeyThatEndsInANewlineByte(t *testing.T) {
	t.Parallel()

	// The key createSecret writes is raw random bytes, so its last byte is
	// '\n' or '\r' once in 128. Trimming those took a byte off a key that was
	// already the right length, and the next start refused it as too short.
	for _, last := range []byte{'\n', '\r'} {
		path := filepath.Join(t.TempDir(), "jwt.key")
		want := append(bytes.Repeat([]byte("k"), auth.SecretSize-1), last)
		writeBytes(t, path, want, 0o600)

		secret, err := auth.LoadOrCreateSecret(path)
		if err != nil {
			t.Errorf("LoadOrCreateSecret() with a key ending in %q: error = %v", last, err)
			continue
		}
		if !bytes.Equal(secret, want) {
			t.Errorf("secret = %q, want all %d bytes back", secret, len(want))
		}
	}
}

func TestGenerateSecret(t *testing.T) {
	t.Parallel()

	first, err := auth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}
	second, err := auth.GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret() error = %v", err)
	}

	if len(first) != auth.SecretSize {
		t.Errorf("secret is %d bytes, want %d", len(first), auth.SecretSize)
	}
	if string(first) == string(second) {
		t.Error("two generated keys are identical")
	}
}

// writeBytes writes a key the way createSecret does: raw bytes, no encoding.
func writeBytes(t *testing.T, path string, contents []byte, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, contents, mode); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
}

func writeFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()

	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
	// WriteFile only applies the mode when it creates the file, and umask can
	// clear bits, so set it explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %q: %v", path, err)
	}
}
