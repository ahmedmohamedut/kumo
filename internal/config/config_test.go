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
		DefaultVPCCIDR:   config.DefaultVPCCIDR,
		JWTSecretFile:    "/var/lib/kumo/jwt.key",
		AccessTTL:        15 * time.Minute,
		RefreshTTL:       168 * time.Hour,
		CookieSecure:     true,
		Workers:          4,
		LogLevel:         slog.LevelInfo,
		StopTimeout:      config.DefaultStopTimeout,
		MemoryReserveMiB: config.DefaultMemoryReserveMiB,
		VCPUOvercommit:   config.DefaultVCPUOvercommit,
		Databases:        config.DefaultDatabases,
		DBProbeInterval:  config.DefaultDBProbeInterval,
		DBReadyTimeout:   config.DefaultDBReadyTimeout,
		ObjectStore:      config.ObjectStoreNone,
		S3Region:         "us-east-1",
		S3PresignTTL:     15 * time.Minute,
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
				if cfg.DefaultVPCCIDR != config.DefaultVPCCIDR {
					t.Errorf("DefaultVPCCIDR = %q, want the default %q", cfg.DefaultVPCCIDR, config.DefaultVPCCIDR)
				}
			},
		},
		{
			name: "default vpc cidr override",
			env:  map[string]string{"KUMO_DEFAULT_VPC_CIDR": "172.31.0.0/16"},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.DefaultVPCCIDR != "172.31.0.0/16" {
					t.Errorf("DefaultVPCCIDR = %q, want 172.31.0.0/16", cfg.DefaultVPCCIDR)
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
			name: "database probe timings",
			env: map[string]string{
				"KUMO_DB_PROBE_INTERVAL": "2s",
				"KUMO_DB_READY_TIMEOUT":  "4m",
			},
			check: func(t *testing.T, cfg *config.Config) {
				t.Helper()
				if cfg.DBProbeInterval != 2*time.Second {
					t.Errorf("DBProbeInterval = %s, want 2s", cfg.DBProbeInterval)
				}
				if cfg.DBReadyTimeout != 4*time.Minute {
					t.Errorf("DBReadyTimeout = %s, want 4m", cfg.DBReadyTimeout)
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
		{name: "db probe interval not a duration", env: map[string]string{
			"KUMO_DB_PROBE_INTERVAL": "ten seconds",
		}},
		{name: "zero db probe interval", env: map[string]string{"KUMO_DB_PROBE_INTERVAL": "0s"}},
		{name: "db ready timeout not a duration", env: map[string]string{
			"KUMO_DB_READY_TIMEOUT": "ten minutes",
		}},
		// A timeout inside one probe interval would fail every create before
		// the first dial it is meant to wait for.
		{name: "db ready timeout shorter than the probe interval", env: map[string]string{
			"KUMO_DB_PROBE_INTERVAL": "30s",
			"KUMO_DB_READY_TIMEOUT":  "10s",
		}},
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
		{name: "default vpc cidr not a cidr", env: map[string]string{"KUMO_DEFAULT_VPC_CIDR": "172.31.0.0"}},
		{name: "default vpc cidr not rfc1918", env: map[string]string{"KUMO_DEFAULT_VPC_CIDR": "8.8.8.0/24"}},
		{name: "default vpc cidr too large", env: map[string]string{"KUMO_DEFAULT_VPC_CIDR": "172.31.0.0/15"}},
		{name: "default vpc cidr has host bits", env: map[string]string{"KUMO_DEFAULT_VPC_CIDR": "172.31.0.5/16"}},
		{name: "unknown log level", env: map[string]string{"KUMO_LOG_LEVEL": "trace"}},
		{name: "unknown object store", env: map[string]string{"KUMO_OBJECTSTORE": "minio"}},
		{name: "rustfs without an endpoint", env: map[string]string{"KUMO_OBJECTSTORE": "rustfs"}},
		{name: "rustfs endpoint without a scheme", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "127.0.0.1:9000",
		}},
		{name: "rustfs endpoint with credentials", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://admin:secret@127.0.0.1:9000",
		}},
		{name: "rustfs bad public url", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://127.0.0.1:9000", "KUMO_S3_PUBLIC_URL": "s3.lan",
		}},
		{name: "rustfs empty region", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://127.0.0.1:9000", "KUMO_S3_REGION": " ",
		}},
		{name: "presign ttl not a duration", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://127.0.0.1:9000", "KUMO_S3_PRESIGN_TTL": "soon",
		}},
		{name: "presign ttl over the sigv4 limit", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://127.0.0.1:9000", "KUMO_S3_PRESIGN_TTL": "169h",
		}},
		{name: "cors origin with a path", env: map[string]string{
			"KUMO_OBJECTSTORE": "rustfs", "KUMO_S3_ENDPOINT": "http://127.0.0.1:9000",
			"KUMO_S3_CORS_ORIGINS": "https://kumo.lan/console",
		}},
		{name: "databases neither on nor off", env: map[string]string{"KUMO_DATABASES": "maybe"}},
		{name: "databases empty", env: map[string]string{"KUMO_DATABASES": ""}},
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

func TestLoadObjectStore(t *testing.T) {
	t.Run("rustfs with defaults", func(t *testing.T) {
		clearKumoEnv(t)
		t.Setenv("KUMO_DATA_DIR", "/srv/kumo")
		t.Setenv("KUMO_OBJECTSTORE", "rustfs")
		t.Setenv("KUMO_S3_ENDPOINT", "http://127.0.0.1:9000/")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if cfg.S3Endpoint != "http://127.0.0.1:9000" || cfg.S3PublicURL != cfg.S3Endpoint {
			t.Errorf("endpoint %q, public URL %q: want the trimmed endpoint for both", cfg.S3Endpoint, cfg.S3PublicURL)
		}
		if cfg.S3CredentialsFile != "/srv/kumo/s3.credentials" {
			t.Errorf("S3CredentialsFile = %q, want derived from KUMO_DATA_DIR", cfg.S3CredentialsFile)
		}
		if cfg.S3Region != "us-east-1" || cfg.S3PresignTTL != 15*time.Minute || cfg.S3CORSOrigins != nil {
			t.Errorf("defaults: region %q, ttl %s, origins %v", cfg.S3Region, cfg.S3PresignTTL, cfg.S3CORSOrigins)
		}
	})

	t.Run("rustfs with every variable", func(t *testing.T) {
		clearKumoEnv(t)
		t.Setenv("KUMO_OBJECTSTORE", "rustfs")
		t.Setenv("KUMO_S3_ENDPOINT", "http://127.0.0.1:9000")
		t.Setenv("KUMO_S3_PUBLIC_URL", "https://s3.kumo.lan")
		t.Setenv("KUMO_S3_REGION", "kumo-1")
		t.Setenv("KUMO_S3_CREDENTIALS_FILE", "/etc/kumo/s3.credentials")
		t.Setenv("KUMO_S3_CORS_ORIGINS", " https://kumo.lan/ , http://localhost:5173,*")
		t.Setenv("KUMO_S3_PRESIGN_TTL", "1h")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		want := []string{"https://kumo.lan", "http://localhost:5173", "*"}
		if !reflect.DeepEqual(cfg.S3CORSOrigins, want) {
			t.Errorf("S3CORSOrigins = %v, want %v", cfg.S3CORSOrigins, want)
		}
		if cfg.S3PublicURL != "https://s3.kumo.lan" || cfg.S3Region != "kumo-1" ||
			cfg.S3CredentialsFile != "/etc/kumo/s3.credentials" || cfg.S3PresignTTL != time.Hour {
			t.Errorf("Load() = %+v", cfg)
		}
	})

	t.Run("off ignores a half-written configuration", func(t *testing.T) {
		clearKumoEnv(t)
		t.Setenv("KUMO_S3_ENDPOINT", "not a url")
		t.Setenv("KUMO_S3_PRESIGN_TTL", "999h")

		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() error = %v, want the module off and the rest loaded", err)
		}
		if cfg.ObjectStore != config.ObjectStoreNone || cfg.S3CredentialsFile != "" {
			t.Errorf("ObjectStore %q, credentials %q: want none and unset", cfg.ObjectStore, cfg.S3CredentialsFile)
		}
	})

	t.Run("fake needs no endpoint", func(t *testing.T) {
		clearKumoEnv(t)
		t.Setenv("KUMO_OBJECTSTORE", "fake")
		if _, err := config.Load(); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
	})
}

// The capacity package carries its own defaults so it can be used without the
// config, and kumod overrides them with the config on boot. The two copies have to
// agree, or a service built directly in a test would enforce different rules than
// the running node.
func TestCapacityDefaultsMatchTheConfiguration(t *testing.T) {
	// Not parallel: clearKumoEnv uses t.Setenv, which Go refuses to combine
	// with t.Parallel. It only panicked on a machine that had a KUMO_*
	// variable set, because that is when clearKumoEnv has anything to clear.
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

// KUMO_DATABASES takes the documented on/off and the booleans strconv
// understands, so an operator who writes true in one Kumo variable does not
// have to remember that this one is spelled differently
// (docs/design/07-databases.md section 2).
func TestLoadDatabasesSwitch(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"on", true},
		{"ON", true},
		{" on ", true},
		{"true", true},
		{"1", true},
		{"off", false},
		{"OFF", false},
		{"false", false},
		{"0", false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			clearKumoEnv(t)
			t.Setenv("KUMO_DATABASES", tt.value)

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if cfg.Databases != tt.want {
				t.Errorf("Databases = %t, want %t", cfg.Databases, tt.want)
			}
		})
	}
}

// The module needs no outside server, so unlike object storage it is on unless
// an operator turns it off.
func TestDatabasesAreOnByDefault(t *testing.T) {
	clearKumoEnv(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if !cfg.Databases {
		t.Error("Databases = false, want true by default")
	}
}
