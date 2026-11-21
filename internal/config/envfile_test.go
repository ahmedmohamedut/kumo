package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/config"
)

func TestApplyEnvFile(t *testing.T) {
	clearKumoEnv(t)
	path := filepath.Join(t.TempDir(), "kumod.env")
	content := `# comment
; another comment

KUMO_OBJECTSTORE=rustfs
  KUMO_S3_ENDPOINT = http://172.26.85.6:9000
KUMO_LOG_LEVEL="debug"
KUMO_LISTEN=':9090'
KUMO_DATA_DIR=/from/file
KUMO_ENV_FILE=/elsewhere
PATH=/evil
not an assignment
KUMO_S3_CORS_ORIGINS=http://a:8080,http://b:8080
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// ApplyEnvFile calls os.Setenv; registering each key with t.Setenv first
	// has the test restore them.
	for _, key := range []string{"KUMO_OBJECTSTORE", "KUMO_S3_ENDPOINT", "KUMO_LOG_LEVEL", "KUMO_LISTEN", "KUMO_S3_CORS_ORIGINS", "KUMO_ENV_FILE"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("KUMO_DATA_DIR", "/from/env")
	t.Setenv("PATH", os.Getenv("PATH"))

	read, err := config.ApplyEnvFile(path)
	if err != nil || !read {
		t.Fatalf("ApplyEnvFile = %v, %v; want true, nil", read, err)
	}

	want := map[string]string{
		"KUMO_OBJECTSTORE":     "rustfs",
		"KUMO_S3_ENDPOINT":     "http://172.26.85.6:9000",
		"KUMO_LOG_LEVEL":       "debug",
		"KUMO_LISTEN":          ":9090",
		"KUMO_DATA_DIR":        "/from/env",
		"KUMO_S3_CORS_ORIGINS": "http://a:8080,http://b:8080",
	}
	for key, value := range want {
		if got := os.Getenv(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if _, set := os.LookupEnv("KUMO_ENV_FILE"); set {
		t.Error("KUMO_ENV_FILE was taken from the file")
	}
	if os.Getenv("PATH") == "/evil" {
		t.Error("a non-KUMO_ variable was taken from the file")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() after the file: %v", err)
	}
	if cfg.ObjectStore != config.ObjectStoreRustFS {
		t.Errorf("ObjectStore = %q, want rustfs", cfg.ObjectStore)
	}
}

func TestApplyEnvFileMissingOrDisabled(t *testing.T) {
	clearKumoEnv(t)
	for _, path := range []string{"", filepath.Join(t.TempDir(), "absent.env")} {
		read, err := config.ApplyEnvFile(path)
		if err != nil || read {
			t.Errorf("ApplyEnvFile(%q) = %v, %v; want false, nil", path, read, err)
		}
	}
}

func TestApplyEnvFileUnreadable(t *testing.T) {
	clearKumoEnv(t)
	if os.Geteuid() == 0 {
		t.Skip("root reads any file")
	}
	path := filepath.Join(t.TempDir(), "kumod.env")
	if err := os.WriteFile(path, []byte("KUMO_LISTEN=:1\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := config.ApplyEnvFile(path); err == nil {
		t.Error("ApplyEnvFile on an unreadable file: want an error")
	}
}

func TestEnvFilePath(t *testing.T) {
	clearKumoEnv(t)
	if got := config.EnvFilePath(); got != config.DefaultEnvFile {
		t.Errorf("EnvFilePath() = %q, want the default", got)
	}
	t.Setenv("KUMO_ENV_FILE", "")
	if got := config.EnvFilePath(); got != "" {
		t.Errorf("EnvFilePath() with KUMO_ENV_FILE empty = %q, want none", got)
	}
	t.Setenv("KUMO_ENV_FILE", "/tmp/x.env")
	if got := config.EnvFilePath(); got != "/tmp/x.env" {
		t.Errorf("EnvFilePath() = %q", got)
	}
}
