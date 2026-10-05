package config_test

import (
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
)

// Config is read from the process environment, so these tests mutate the
// environment with t.Setenv and must not run in parallel.

func TestLoadDefaults(t *testing.T) {
	clearKumoEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	want := config.Config{
		Listen:           ":8080",
		AllowedOrigins:   nil,
		DataDir:          "/var/lib/kumo",
		DBPath:           "/var/lib/kumo/kumo.db",
		ImageMaxBytes:    20 << 30,
		ImageTempDir:     "/var/lib/kumo/staging",
		Hypervisor:       config.HypervisorLibvirt,
		LibvirtURI:       "unix:///var/run/libvirt/libvirt-sock",
		PoolImages:       "kumo-images",
		PoolInstances:    "kumo-instances",
		Network:          "kumo-default",
		JWTSecretFile:    "/var/lib/kumo/jwt.key",
		AccessTTL:        15 * time.Minute,
		RefreshTTL:       168 * time.Hour,
		CookieSecure:     true,
		Workers:          4,
		LogLevel:         slog.LevelInfo,
		StopTimeout:      config.DefaultStopTimeout,
		MemoryReserveMiB: config.DefaultMemoryReserveMiB,
		VCPUOvercommit:   config.DefaultVCPUOvercommit,
	}

	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("Load() = %+v, want %+v", *cfg, want)
	}
}

func TestLoadFromEnv(t *testing.T) {

	tests := []struct {
		name  string
		env   map[string]string
		check func(t *testing.T, cfg *config.Config)
	}{
		{
			name: "listen and data dir",
			env:  map[string]string{"KUMO_LISTEN": "127.0.0.1:9000", "KUMO_DATA_DIR": "/srv/kumo"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.Listen != "127.0.0.1:9000" {
					t.Errorf("Listen = %q", cfg.Listen)
				}
				if cfg.DBPath != "/srv/kumo/kumo.db" {
					t.Errorf("DBPath = %q, want derived from KUMO_DATA_DIR", cfg.DBPath)
				}
				if cfg.JWTSecretFile != "/srv/kumo/jwt.key" {
					t.Errorf("JWTSecretFile = %q, want derived from KUMO_DATA_DIR", cfg.JWTSecretFile)
				}
			},
		},
		{
			name: "explicit db path wins over data dir",
			env: map[string]string{
				"KUMO_DATA_DIR": "/srv/kumo",
				"KUMO_DB_PATH":  "/var/lib/other/state.db",
			},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.DBPath != "/var/lib/other/state.db" {
					t.Errorf("DBPath = %q", cfg.DBPath)
				}
			},
		},
		{
			name: "fake hypervisor",
			env:  map[string]string{"KUMO_HYPERVISOR": "fake"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.Hypervisor != config.HypervisorFake {
					t.Errorf("Hypervisor = %q", cfg.Hypervisor)
				}
			},
		},
		{
			name: "libvirt resources",
			env: map[string]string{
				"KUMO_LIBVIRT_URI":    "unix:///tmp/libvirt.sock",
				"KUMO_POOL_IMAGES":    "other-images",
				"KUMO_POOL_INSTANCES": "other-instances",
				"KUMO_NETWORK":        "virbr0",
			},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.LibvirtURI != "unix:///tmp/libvirt.sock" {
					t.Errorf("LibvirtURI = %q", cfg.LibvirtURI)
				}
				if cfg.PoolImages != "other-images" || cfg.PoolInstances != "other-instances" {
					t.Errorf("pools = %q/%q", cfg.PoolImages, cfg.PoolInstances)
				}
				if cfg.Network != "virbr0" {
					t.Errorf("Network = %q", cfg.Network)
				}
			},
		},
		{
			name: "token lifetimes",
			env: map[string]string{
				"KUMO_ACCESS_TTL":  "5m",
				"KUMO_REFRESH_TTL": "24h",
			},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.AccessTTL != 5*time.Minute {
					t.Errorf("AccessTTL = %s", cfg.AccessTTL)
				}
				if cfg.RefreshTTL != 24*time.Hour {
					t.Errorf("RefreshTTL = %s", cfg.RefreshTTL)
				}
			},
		},
		{
			name: "stop timeout",
			env:  map[string]string{"KUMO_STOP_TIMEOUT": "30s"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.StopTimeout != 30*time.Second {
					t.Errorf("StopTimeout = %s, want 30s", cfg.StopTimeout)
				}
			},
		},
		{
			name: "cookie secure off for http dev",
			env:  map[string]string{"KUMO_COOKIE_SECURE": "false"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.CookieSecure {
					t.Error("CookieSecure = true, want false")
				}
			},
		},
		{
			name: "workers",
			env:  map[string]string{"KUMO_WORKERS": "8"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.Workers != 8 {
					t.Errorf("Workers = %d", cfg.Workers)
				}
			},
		},
		{
			name: "image max bytes with unit",
			env:  map[string]string{"KUMO_IMAGE_MAX_BYTES": "5GiB"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.ImageMaxBytes != 5<<30 {
					t.Errorf("ImageMaxBytes = %d", cfg.ImageMaxBytes)
				}
			},
		},
		{
			name: "image temp dir",
			env:  map[string]string{"KUMO_IMAGE_TEMP_DIR": "/srv/kumo/staging"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.ImageTempDir != "/srv/kumo/staging" {
					t.Errorf("ImageTempDir = %q", cfg.ImageTempDir)
				}
			},
		},
		{
			name: "image private urls refused by default",
			env:  map[string]string{},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.ImageAllowPrivateURLs {
					t.Error("ImageAllowPrivateURLs = true, want false by default")
				}
			},
		},
		{
			name: "image private urls opt in",
			env:  map[string]string{"KUMO_IMAGE_ALLOW_PRIVATE_URLS": "true"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if !cfg.ImageAllowPrivateURLs {
					t.Error("ImageAllowPrivateURLs = false, want true")
				}
			},
		},
		{
			name: "memory reserve",
			env:  map[string]string{"KUMO_MEMORY_RESERVE_MIB": "4096"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.MemoryReserveMiB != 4096 {
					t.Errorf("MemoryReserveMiB = %d, want 4096", cfg.MemoryReserveMiB)
				}
			},
		},
		{
			name: "vcpu overcommit",
			env:  map[string]string{"KUMO_VCPU_OVERCOMMIT": "2"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.VCPUOvercommit != 2 {
					t.Errorf("VCPUOvercommit = %d, want 2", cfg.VCPUOvercommit)
				}
			},
		},
		{
			name: "log level",
			env:  map[string]string{"KUMO_LOG_LEVEL": "debug"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.LogLevel != slog.LevelDebug {
					t.Errorf("LogLevel = %s", cfg.LogLevel)
				}
			},
		},
		{
			name: "allowed origins",
			env:  map[string]string{"KUMO_ALLOWED_ORIGINS": "http://localhost:5173, https://kumo.example"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				want := []string{"http://localhost:5173", "https://kumo.example"}
				if len(cfg.AllowedOrigins) != len(want) {
					t.Fatalf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, want)
				}
				for i := range want {
					if cfg.AllowedOrigins[i] != want[i] {
						t.Errorf("AllowedOrigins[%d] = %q, want %q", i, cfg.AllowedOrigins[i], want[i])
					}
				}
			},
		},
		{
			name: "empty allowed origins",
			env:  map[string]string{"KUMO_ALLOWED_ORIGINS": " , "},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if len(cfg.AllowedOrigins) != 0 {
					t.Errorf("AllowedOrigins = %v, want empty", cfg.AllowedOrigins)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearKumoEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {

	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "unknown hypervisor", env: map[string]string{"KUMO_HYPERVISOR": "vmware"}},
		{name: "empty listen", env: map[string]string{"KUMO_LISTEN": ""}},
		{name: "empty data dir", env: map[string]string{"KUMO_DATA_DIR": ""}},
		{name: "zero workers", env: map[string]string{"KUMO_WORKERS": "0"}},
		{name: "negative workers", env: map[string]string{"KUMO_WORKERS": "-1"}},
		{name: "workers not a number", env: map[string]string{"KUMO_WORKERS": "many"}},
		{name: "access ttl not a duration", env: map[string]string{"KUMO_ACCESS_TTL": "15 minutes"}},
		{name: "refresh ttl shorter than access ttl", env: map[string]string{
			"KUMO_ACCESS_TTL":  "1h",
			"KUMO_REFRESH_TTL": "30m",
		}},
		{name: "negative access ttl", env: map[string]string{"KUMO_ACCESS_TTL": "-1m", "KUMO_REFRESH_TTL": "1h"}},
		{name: "stop timeout not a duration", env: map[string]string{"KUMO_STOP_TIMEOUT": "90 seconds"}},
		{name: "zero stop timeout", env: map[string]string{"KUMO_STOP_TIMEOUT": "0s"}},
		{name: "negative stop timeout", env: map[string]string{"KUMO_STOP_TIMEOUT": "-90s"}},
		{name: "cookie secure not a bool", env: map[string]string{"KUMO_COOKIE_SECURE": "yes-please"}},
		{name: "image max bytes not a size", env: map[string]string{"KUMO_IMAGE_MAX_BYTES": "20 gigs"}},
		{name: "negative image max bytes", env: map[string]string{"KUMO_IMAGE_MAX_BYTES": "-1"}},
		{
			name: "image private urls not a bool",
			env:  map[string]string{"KUMO_IMAGE_ALLOW_PRIVATE_URLS": "yes please"},
		},
		{name: "negative memory reserve", env: map[string]string{"KUMO_MEMORY_RESERVE_MIB": "-1"}},
		{name: "memory reserve not a number", env: map[string]string{"KUMO_MEMORY_RESERVE_MIB": "half"}},
		{name: "zero vcpu overcommit", env: map[string]string{"KUMO_VCPU_OVERCOMMIT": "0"}},
		{name: "negative vcpu overcommit", env: map[string]string{"KUMO_VCPU_OVERCOMMIT": "-2"}},
		{name: "vcpu overcommit not a number", env: map[string]string{"KUMO_VCPU_OVERCOMMIT": "many"}},
		{name: "unknown log level", env: map[string]string{"KUMO_LOG_LEVEL": "trace"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearKumoEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			if _, err := config.Load(); err == nil {
				t.Fatal("Load() error = nil, want an error")
			}
		})
	}
}

// The capacity package carries its own defaults so it can be used without the
// config, and kumod overrides them with the config on boot. The two copies have to
// agree, or a service built directly in a test would enforce different rules than
// the running node.
func TestCapacityDefaultsMatchTheConfiguration(t *testing.T) {
	t.Parallel()

	clearKumoEnv(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if got, want := cfg.MemoryReserveMiB, capacity.DefaultLimits().MemoryReserveMiB; got != want {
		t.Errorf("DefaultMemoryReserveMiB = %d, but internal/service/capacity defaults to %d", got, want)
	}
	if got, want := cfg.VCPUOvercommit, capacity.DefaultLimits().VCPUOvercommit; got != want {
		t.Errorf("DefaultVCPUOvercommit = %d, but internal/service/capacity defaults to %d", got, want)
	}
}

func TestParseBytes(t *testing.T) {

	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "0", want: 0},
		{in: "1024", want: 1024},
		{in: "1KiB", want: 1024},
		{in: "2MiB", want: 2 << 20},
		{in: "20gib", want: 20 << 30},
		{in: "1 TiB", want: 1 << 40},
		{in: "512B", want: 512},
		{in: "", wantErr: true},
		{in: "-5", wantErr: true},
		{in: "5ZiB", wantErr: true},
		{in: "many", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := config.ParseBytes(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseBytes(%q) = %d, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBytes(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseBytes(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseLogLevel(t *testing.T) {

	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "INFO", want: slog.LevelInfo},
		{in: " warn ", want: slog.LevelWarn},
		{in: "warning", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "", want: slog.LevelInfo},
		{in: "verbose", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := config.ParseLogLevel(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseLogLevel(%q) = %s, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseLogLevel(%q) error = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseLogLevel(%q) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// clearKumoEnv removes every KUMO_* variable for the duration of the test so
// the host environment cannot influence the result.
func clearKumoEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, "KUMO_") {
			continue
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("os.Unsetenv(%q) error = %v", key, err)
		}
	}
}
