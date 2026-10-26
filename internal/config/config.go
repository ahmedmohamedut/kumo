// Package config loads the Kumo configuration from KUMO_* environment
// variables, with the defaults documented in docs/design/00-architecture.md.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/network"
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
	// ErrInvalidObjectStore means KUMO_OBJECTSTORE names no known backend, or
	// the backend it names is missing a setting it needs.
	ErrInvalidObjectStore = errors.New("invalid object store")
)

// Hypervisor backends.
const (
	HypervisorLibvirt = "libvirt"
	HypervisorFake    = "fake"
)

// Object storage backends. ObjectStoreNone leaves the module off: the
// /api/v1/s3 routes answer 503 and the console shows it as not configured
// (docs/design/06-object-storage.md §2).
const (
	ObjectStoreNone   = "none"
	ObjectStoreRustFS = "rustfs"
	ObjectStoreFake   = "fake"
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
	Hypervisor     string
	LibvirtURI     string
	PoolImages     string
	PoolInstances  string
	Network        string
	DefaultVPCCIDR string

	// Auth
	JWTSecretFile string
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	CookieSecure  bool

	// Object storage. See docs/design/06-object-storage.md §2.
	ObjectStore string
	// S3Endpoint is where kumod reaches the S3 server.
	S3Endpoint string
	// S3PublicURL is where a browser reaches it; presigned URLs are signed for
	// this host. It defaults to S3Endpoint.
	S3PublicURL string
	S3Region    string
	// S3CredentialsFile holds the server's admin access key and secret key.
	// It is read at startup and must not be readable by group or others.
	S3CredentialsFile string
	// S3CORSOrigins are the console origins a bucket's CORS rule allows, so a
	// browser can upload through a presigned URL.
	S3CORSOrigins []string
	// S3PresignTTL is how long a presigned URL stays valid.
	S3PresignTTL time.Duration

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
	// DefaultVPCCIDR is the range of the default VPC a fresh boot creates. It is
	// the guest range of docs/design/04-libvirt-integration.md §3; an operator
	// whose LAN already owns that block overrides it with KUMO_DEFAULT_VPC_CIDR,
	// because a NAT guest network must never shadow a host route.
	DefaultVPCCIDR       = "192.168.100.0/24"
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
	DefaultObjectStore    = ObjectStoreNone
	// DefaultS3Region is signed into every S3 request. RustFS accepts any
	// region; us-east-1 is what clients assume when none is given.
	DefaultS3Region = "us-east-1"
	// DefaultS3CredentialsName is the credentials file under DataDir.
	DefaultS3CredentialsName = "s3.credentials"
	// DefaultS3PresignTTL is long enough to start a large upload on a slow
	// link (the URL must be valid when the request begins, not when it ends)
	// and short enough that a leaked URL is soon worthless.
	DefaultS3PresignTTL = 15 * time.Minute
	// MaxS3PresignTTL is the SigV4 ceiling.
	MaxS3PresignTTL = 7 * 24 * time.Hour
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
		DefaultVPCCIDR:   DefaultVPCCIDR,
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
		ObjectStore:      DefaultObjectStore,
		S3Region:         DefaultS3Region,
		S3PresignTTL:     DefaultS3PresignTTL,
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
	if v, ok := os.LookupEnv("KUMO_DEFAULT_VPC_CIDR"); ok {
		cfg.DefaultVPCCIDR = v
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
	cfg.loadObjectStore()
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
	if cfg.ObjectStore != ObjectStoreNone {
		if cfg.S3CredentialsFile == "" {
			cfg.S3CredentialsFile = filepath.Join(cfg.DataDir, DefaultS3CredentialsName)
		}
		if cfg.S3PublicURL == "" {
			cfg.S3PublicURL = cfg.S3Endpoint
		}
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
	if v, ok := os.LookupEnv("KUMO_S3_PRESIGN_TTL"); ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%w: KUMO_S3_PRESIGN_TTL: %w", ErrInvalidDuration, err)
		}
		c.S3PresignTTL = d
	}
	return nil
}

// loadObjectStore reads the KUMO_OBJECTSTORE and KUMO_S3_* variables.
func (c *Config) loadObjectStore() {
	if v, ok := os.LookupEnv("KUMO_OBJECTSTORE"); ok {
		c.ObjectStore = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("KUMO_S3_ENDPOINT"); ok {
		c.S3Endpoint = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := os.LookupEnv("KUMO_S3_PUBLIC_URL"); ok {
		c.S3PublicURL = strings.TrimRight(strings.TrimSpace(v), "/")
	}
	if v, ok := os.LookupEnv("KUMO_S3_REGION"); ok {
		c.S3Region = strings.TrimSpace(v)
	}
	if v, ok := os.LookupEnv("KUMO_S3_CREDENTIALS_FILE"); ok {
		c.S3CredentialsFile = v
	}
	if v, ok := os.LookupEnv("KUMO_S3_CORS_ORIGINS"); ok {
		for origin := range strings.SplitSeq(v, ",") {
			if origin = strings.TrimRight(strings.TrimSpace(origin), "/"); origin != "" {
				c.S3CORSOrigins = append(c.S3CORSOrigins, origin)
			}
		}
	}
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
	if _, err := network.ValidateVPCCIDR(c.DefaultVPCCIDR); err != nil {
		return fmt.Errorf("%w: KUMO_DEFAULT_VPC_CIDR: %w", ErrInvalidValue, err)
	}
	return c.validateObjectStore()
}

// validateObjectStore checks the object storage settings. With the module
// off, the KUMO_S3_* variables are ignored rather than checked, so a
// half-written configuration does not stop a node that does not use it.
func (c *Config) validateObjectStore() error {
	switch c.ObjectStore {
	case ObjectStoreNone, ObjectStoreFake:
		return nil
	case ObjectStoreRustFS:
	default:
		return fmt.Errorf("%w: KUMO_OBJECTSTORE must be %q, %q or %q, got %q",
			ErrInvalidObjectStore, ObjectStoreNone, ObjectStoreRustFS, ObjectStoreFake, c.ObjectStore)
	}

	if err := checkHTTPURL(c.S3Endpoint, false); err != nil {
		return fmt.Errorf("%w: KUMO_S3_ENDPOINT is required with KUMO_OBJECTSTORE=rustfs: %w",
			ErrInvalidObjectStore, err)
	}
	if err := checkHTTPURL(c.S3PublicURL, false); err != nil {
		return fmt.Errorf("%w: KUMO_S3_PUBLIC_URL: %w", ErrInvalidObjectStore, err)
	}
	if c.S3Region == "" {
		return fmt.Errorf("%w: KUMO_S3_REGION must not be empty", ErrInvalidObjectStore)
	}
	if c.S3CredentialsFile == "" {
		return fmt.Errorf("%w: KUMO_S3_CREDENTIALS_FILE must not be empty", ErrInvalidObjectStore)
	}
	if c.S3PresignTTL < time.Second || c.S3PresignTTL > MaxS3PresignTTL {
		return fmt.Errorf("%w: KUMO_S3_PRESIGN_TTL must be between 1s and %s, got %s",
			ErrInvalidDuration, MaxS3PresignTTL, c.S3PresignTTL)
	}
	for _, origin := range c.S3CORSOrigins {
		if origin == "*" {
			continue
		}
		if err := checkHTTPURL(origin, true); err != nil {
			return fmt.Errorf("%w: KUMO_S3_CORS_ORIGINS: %w", ErrInvalidObjectStore, err)
		}
	}
	return nil
}

// checkHTTPURL accepts an absolute http(s) URL. An origin is a URL with no
// path at all, the form a browser sends in its Origin header.
func checkHTTPURL(raw string, origin bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q: %w", raw, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q is not an absolute http(s) URL", ErrInvalidValue, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%w: %q must not carry credentials, a query or a fragment", ErrInvalidValue, raw)
	}
	if origin && u.Path != "" {
		return fmt.Errorf("%w: %q is not an origin: drop the path", ErrInvalidValue, raw)
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
