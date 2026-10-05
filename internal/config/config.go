// Package config loads the Kumo configuration from KUMO_* environment
// variables, with the defaults documented in docs/design/00-architecture.md.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors so callers (and tests) can classify configuration problems
// without matching on message text.
var (
	// ErrInvalidHypervisor means KUMO_HYPERVISOR names no known backend.
	ErrInvalidHypervisor = errors.New("invalid hypervisor")
	// ErrInvalidValue means a variable is empty or otherwise unusable.
	ErrInvalidValue = errors.New("invalid value")
	// ErrInvalidWorkers means KUMO_WORKERS is not a usable worker count.
	ErrInvalidWorkers = errors.New("invalid worker count")
	// ErrInvalidDuration means a variable is not a usable Go duration.
	ErrInvalidDuration = errors.New("invalid duration")
	// ErrInvalidSize means a variable is not a usable byte size.
	ErrInvalidSize = errors.New("invalid size")
)

// Hypervisor backends.
const (
	HypervisorLibvirt = "libvirt"
	HypervisorFake    = "fake"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// HTTP
	Listen         string
	AllowedOrigins []string

	// Storage
	DataDir       string
	DBPath        string
	ImageMaxBytes int64
	// ImageTempDir is where a download is staged before it becomes a volume.
	// It defaults to DataDir/staging and must live on the same filesystem as the
	// images pool, so placing a volume is a rename and not a copy.
	ImageTempDir string
	// ImageAllowPrivateURLs lifts the loopback, private and link-local blocks
	// on image downloads. It exists for deployments that mirror images on the
	// LAN and must stay false otherwise, because the console is admin-only but
	// still a browser away from the metadata service.
	ImageAllowPrivateURLs bool

	// Hypervisor
	Hypervisor    string
	LibvirtURI    string
	PoolImages    string
	PoolInstances string
	Network       string

	// Auth
	JWTSecretFile string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	CookieSecure  bool

	// Runtime
	Workers  int
	LogLevel slog.Level

	// StopTimeout is how long a graceful stop waits for the guest to power off
	// before the task gives up and leaves the instance in stopping for the
	// operator to force. It is a wait on a guest, not on the host: a guest that
	// ignores ACPI is still running when the wait is over.
	StopTimeout time.Duration

	// Capacity. The launch check refuses a guest the node cannot hold; see
	// docs/design/00-architecture.md §5 and internal/service/capacity.
	MemoryReserveMiB int64
	VCPUOvercommit   int
}

// Default values, see docs/design/00-architecture.md §5.
const (
	DefaultListen        = ":8080"
	DefaultDataDir       = "/var/lib/kumo"
	DefaultHypervisor    = HypervisorLibvirt
	DefaultLibvirtURI    = "unix:///var/run/libvirt/libvirt-sock"
	DefaultPoolImages    = "kumo-images"
	DefaultPoolInstances = "kumo-instances"
	DefaultNetwork       = "kumo-default"
	DefaultAccessTTL     = 15 * time.Minute
	DefaultRefreshTTL    = 168 * time.Hour
	DefaultCookieSecure  = true
	DefaultWorkers       = 4
	DefaultImageMaxBytes = int64(20) << 30 // 20 GiB
	// DefaultStopTimeout is the watchdog of a graceful stop. Ninety seconds is
	// longer than the shutdown of a cloud image that is doing what it was asked
	// (a systemd unit, a service that refuses to die) and shorter than an
	// operator is willing to watch a spinner.
	DefaultStopTimeout = 90 * time.Second
	// DefaultMemoryReserveMiB is the memory a node keeps for itself, so a launch
	// never hands the last of the host's RAM to a guest.
	DefaultMemoryReserveMiB = int64(1024)
	// DefaultVCPUOvercommit is how many guest vCPUs one host vCPU may carry.
	DefaultVCPUOvercommit = 4
	// DefaultStagingDirName is the directory under DataDir that stages downloads.
	DefaultStagingDirName = "staging"
	DefaultLogLevel       = slog.LevelInfo
)

// Load reads the configuration from the process environment, applies the
// documented defaults and validates the result.
func Load() (*Config, error) {
	cfg := &Config{
		Listen:           DefaultListen,
		DataDir:          DefaultDataDir,
		Hypervisor:       DefaultHypervisor,
		LibvirtURI:       DefaultLibvirtURI,
		PoolImages:       DefaultPoolImages,
		PoolInstances:    DefaultPoolInstances,
		Network:          DefaultNetwork,
		AccessTTL:        DefaultAccessTTL,
		RefreshTTL:       DefaultRefreshTTL,
		StopTimeout:      DefaultStopTimeout,
		CookieSecure:     DefaultCookieSecure,
		Workers:          DefaultWorkers,
		ImageMaxBytes:    DefaultImageMaxBytes,
		MemoryReserveMiB: DefaultMemoryReserveMiB,
		VCPUOvercommit:   DefaultVCPUOvercommit,
		LogLevel:         DefaultLogLevel,
		AllowedOrigins:   nil,
	}

	if v, ok := os.LookupEnv("KUMO_LISTEN"); ok {
		cfg.Listen = v
	}
	if v, ok := os.LookupEnv("KUMO_DATA_DIR"); ok {
		cfg.DataDir = v
	}
	if v, ok := os.LookupEnv("KUMO_HYPERVISOR"); ok {
		cfg.Hypervisor = v
	}
	if v, ok := os.LookupEnv("KUMO_LIBVIRT_URI"); ok {
		cfg.LibvirtURI = v
	}
	if v, ok := os.LookupEnv("KUMO_POOL_IMAGES"); ok {
		cfg.PoolImages = v
	}
	if v, ok := os.LookupEnv("KUMO_POOL_INSTANCES"); ok {
		cfg.PoolInstances = v
	}
	if v, ok := os.LookupEnv("KUMO_NETWORK"); ok {
		cfg.Network = v
	}
	if v, ok := os.LookupEnv("KUMO_COOKIE_SECURE"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_COOKIE_SECURE: %w", ErrInvalidValue, err)
		}
		cfg.CookieSecure = b
	}
	if v, ok := os.LookupEnv("KUMO_JWT_SECRET_FILE"); ok {
		cfg.JWTSecretFile = v
	}
	if v, ok := os.LookupEnv("KUMO_WORKERS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_WORKERS: %w", ErrInvalidWorkers, err)
		}
		cfg.Workers = n
	}
	if v, ok := os.LookupEnv("KUMO_IMAGE_MAX_BYTES"); ok {
		n, err := ParseBytes(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_IMAGE_MAX_BYTES: %w", ErrInvalidSize, err)
		}
		cfg.ImageMaxBytes = n
	}
	if v, ok := os.LookupEnv("KUMO_IMAGE_TEMP_DIR"); ok {
		cfg.ImageTempDir = v
	}
	if v, ok := os.LookupEnv("KUMO_IMAGE_ALLOW_PRIVATE_URLS"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_IMAGE_ALLOW_PRIVATE_URLS: %w", ErrInvalidValue, err)
		}
		cfg.ImageAllowPrivateURLs = b
	}
	if v, ok := os.LookupEnv("KUMO_MEMORY_RESERVE_MIB"); ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_MEMORY_RESERVE_MIB: %w", ErrInvalidValue, err)
		}
		cfg.MemoryReserveMiB = n
	}
	if v, ok := os.LookupEnv("KUMO_VCPU_OVERCOMMIT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_VCPU_OVERCOMMIT: %w", ErrInvalidValue, err)
		}
		cfg.VCPUOvercommit = n
	}
	if v, ok := os.LookupEnv("KUMO_LOG_LEVEL"); ok {
		level, err := ParseLogLevel(v)
		if err != nil {
			return nil, fmt.Errorf("%w: KUMO_LOG_LEVEL: %w", ErrInvalidValue, err)
		}
		cfg.LogLevel = level
	}
	if err := cfg.loadDurations(); err != nil {
		return nil, err
	}
	if err := cfg.loadOrigins(); err != nil {
		return nil, err
	}
	cfg.DBPath = filepath.Join(cfg.DataDir, "kumo.db")
	if v, ok := os.LookupEnv("KUMO_DB_PATH"); ok {
		cfg.DBPath = v
	}
	if cfg.JWTSecretFile == "" {
		cfg.JWTSecretFile = filepath.Join(cfg.DataDir, "jwt.key")
	}
	if cfg.ImageTempDir == "" {
		cfg.ImageTempDir = filepath.Join(cfg.DataDir, DefaultStagingDirName)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) loadDurations() error {
	if v, ok := os.LookupEnv("KUMO_ACCESS_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: KUMO_ACCESS_TTL: %w", ErrInvalidDuration, err)
		}
		c.AccessTTL = d
	}
	if v, ok := os.LookupEnv("KUMO_REFRESH_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: KUMO_REFRESH_TTL: %w", ErrInvalidDuration, err)
		}
		c.RefreshTTL = d
	}
	if v, ok := os.LookupEnv("KUMO_STOP_TIMEOUT"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: KUMO_STOP_TIMEOUT: %w", ErrInvalidDuration, err)
		}
		c.StopTimeout = d
	}
	return nil
}

func (c *Config) loadOrigins() error {
	v, ok := os.LookupEnv("KUMO_ALLOWED_ORIGINS")
	if !ok {
		return nil
	}
	for origin := range strings.SplitSeq(v, ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		c.AllowedOrigins = append(c.AllowedOrigins, origin)
	}
	return nil
}

// Validate reports values that would make the process misbehave later.
func (c *Config) Validate() error {
	switch c.Hypervisor {
	case HypervisorLibvirt, HypervisorFake:
	default:
		return fmt.Errorf("%w: KUMO_HYPERVISOR must be %q or %q, got %q",
			ErrInvalidHypervisor, HypervisorLibvirt, HypervisorFake, c.Hypervisor)
	}
	if c.Listen == "" {
		return fmt.Errorf("%w: KUMO_LISTEN must not be empty", ErrInvalidValue)
	}
	if c.DataDir == "" {
		return fmt.Errorf("%w: KUMO_DATA_DIR must not be empty", ErrInvalidValue)
	}
	if c.Workers < 1 {
		return fmt.Errorf("%w: KUMO_WORKERS must be >= 1, got %d", ErrInvalidWorkers, c.Workers)
	}
	if c.AccessTTL <= 0 {
		return fmt.Errorf("%w: KUMO_ACCESS_TTL must be positive, got %s", ErrInvalidDuration, c.AccessTTL)
	}
	if c.RefreshTTL <= c.AccessTTL {
		return fmt.Errorf("%w: KUMO_REFRESH_TTL must be longer than KUMO_ACCESS_TTL, got %s",
			ErrInvalidDuration, c.RefreshTTL)
	}
	if c.StopTimeout <= 0 {
		return fmt.Errorf("%w: KUMO_STOP_TIMEOUT must be positive, got %s",
			ErrInvalidDuration, c.StopTimeout)
	}
	if c.ImageMaxBytes <= 0 {
		return fmt.Errorf("%w: KUMO_IMAGE_MAX_BYTES must be positive, got %d",
			ErrInvalidSize, c.ImageMaxBytes)
	}
	if c.ImageTempDir == "" {
		return fmt.Errorf("%w: KUMO_IMAGE_TEMP_DIR must not be empty", ErrInvalidValue)
	}
	if c.MemoryReserveMiB < 0 {
		return fmt.Errorf("%w: KUMO_MEMORY_RESERVE_MIB must not be negative, got %d",
			ErrInvalidValue, c.MemoryReserveMiB)
	}
	if c.VCPUOvercommit < 1 {
		return fmt.Errorf("%w: KUMO_VCPU_OVERCOMMIT must be >= 1, got %d", ErrInvalidValue, c.VCPUOvercommit)
	}
	return nil
}

// ParseLogLevel maps a KUMO_LOG_LEVEL value to an slog level.
func ParseLogLevel(v string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("%w: KUMO_LOG_LEVEL must be debug|info|warn|error, got %q",
			ErrInvalidValue, v)
	}
}

// ParseBytes parses a byte size, either a plain integer or one with a
// binary unit suffix (KiB, MiB, GiB, TiB).
func ParseBytes(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, fmt.Errorf("%w: size must not be empty", ErrInvalidSize)
	}

	multipliers := []struct {
		suffix string
		factor int64
	}{
		{"tib", 1 << 40},
		{"gib", 1 << 30},
		{"mib", 1 << 20},
		{"kib", 1 << 10},
		{"tb", 1 << 40},
		{"gb", 1 << 30},
		{"mb", 1 << 20},
		{"kb", 1 << 10},
		{"b", 1},
	}

	lower := strings.ToLower(v)
	for _, m := range multipliers {
		num, ok := strings.CutSuffix(lower, m.suffix)
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %q: %w", ErrInvalidSize, v, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("%w: %q must not be negative", ErrInvalidSize, v)
		}
		return n * m.factor, nil
	}

	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q: %w", ErrInvalidSize, v, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: %q must not be negative", ErrInvalidSize, v)
	}
	return n, nil
}
