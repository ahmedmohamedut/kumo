// Package sparkcloudinit renders what a Kumo Spark cluster is configured by
// (docs/design/09-spark.md sections 4 and 5): the cloud-init user-data a
// master and a worker boot from, and the client configuration an admin
// downloads to submit jobs with.
//
// Like internal/service/k8scloudinit it is free of libvirt, of the database
// and of the services, so every rule is a function with a test. Every value
// that reaches a file is validated against a narrow pattern first, so nothing a
// request supplies can break out of a properties line or an environment file,
// and the first-boot scripts are constants that read the files.
package sparkcloudinit

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const cloudConfigHeader = "#cloud-config"

// Paths and ports on a node, as packaging/images/spark-4.3 lays them out.
const (
	confDir      = "/etc/spark"
	defaultsPath = confDir + "/spark-defaults.conf"
	nodeEnvPath  = confDir + "/kumo-node.env"
	// runDir holds the first-boot script; /run is noexec, so it is run with
	// `bash <path>`.
	runDir     = "/run/kumo-spark"
	scriptPath = runDir + "/firstboot.sh"

	// MasterPort is the master's RPC port, the one spark-submit connects to.
	MasterPort = 7077
	// MasterUIPort serves the master's web UI and its /json/ status.
	MasterUIPort = 8080
	// HistoryPort is the history server's web UI.
	HistoryPort = 18080
)

// Console markers, for whoever reads the serial log of a node that failed.
const (
	markerMasterReady = "KUMO-SPARK-MASTER-STARTED"
	markerWorkerReady = "KUMO-SPARK-WORKER-STARTED"
	markerFailed      = "KUMO-SPARK-FIRSTBOOT-FAILED"
)

// Bounds of what a worker may offer and how long it may drain.
const (
	MaxNameLength      = 63
	MinWorkerCores     = 1
	MaxWorkerCores     = 256
	MinWorkerMemoryMiB = 512
	MinDrainTimeout    = 10 * time.Second
	MaxDrainTimeout    = 2 * time.Hour
	// secretBytes is the Spark auth secret's entropy: 256 bits.
	secretBytes = 32
)

var (
	labelPattern  = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)
	idPattern     = regexp.MustCompile(`^spk-[0-9a-f]{17}$`)
	secretPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	// An access key as Kumo issues them, and secrets as RustFS accepts them:
	// no character a properties file or a shell would read specially.
	accessKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
	secretKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_\-+/=]{16,128}$`)
	regionPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	// A bucket name by S3's rules, the dots included.
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// ErrInvalidRequest means a field could not be rendered safely.
var ErrInvalidRequest = errors.New("sparkcloudinit: the request is not renderable")

// ErrSecretMarshal is what marshaling a Secret to JSON answers.
var ErrSecretMarshal = errors.New("sparkcloudinit: refusing to marshal a secret")

// Secret is secret material: the Spark auth secret, an S3 secret key, or a
// rendered document that carries one. Its String, GoString, LogValue and every
// fmt verb print [redacted], so formatting or logging a struct that holds one
// never prints it; Value is the only way to the contents.
type Secret string

const redacted = "[redacted]"

// Value returns the contents, for the few places that must write them.
func (s Secret) Value() string { return string(s) }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// LogValue keeps slog from printing the secret.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Format covers every fmt verb.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON refuses: a secret has no business in a task payload or a JSON
// response, and one that tried to carry it should fail loudly.
func (s Secret) MarshalJSON() ([]byte, error) { return nil, ErrSecretMarshal }

// NewSecret is a cluster's Spark auth secret: 256 random bits, hex. rnd is
// crypto/rand when nil.
func NewSecret(rnd io.Reader) (Secret, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", fmt.Errorf("sparkcloudinit: read randomness: %w", err)
	}
	return Secret(hex.EncodeToString(b)), nil
}

// S3 is how a cluster reaches Kumo's object storage: the endpoint the guests
// route to (KUMO_SPARK_S3_ENDPOINT) and the cluster's own access key.
type S3 struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey Secret
}

// EventLog is where a cluster's jobs write their event logs and its history
// server reads them: a prefix of the event-log bucket, per cluster.
type EventLog struct {
	S3     S3
	Bucket string
}

// MasterRequest is everything a master's first boot needs.
type MasterRequest struct {
	ClusterID string
	NodeName  string
	NodeIP    string
	Secret    Secret
	// EventLog is nil for a cluster without an event-log bucket: no history
	// server then (09-spark.md S10).
	EventLog *EventLog
}

// WorkerRequest is everything a worker's first boot needs. It carries the
// Spark secret and no S3 credential: a job's S3 settings reach its executors
// from its driver (09-spark.md section 5).
type WorkerRequest struct {
	NodeName string
	NodeIP   string
	MasterIP string
	Secret   Secret
	// Cores and MemoryMiB are what the worker offers executors.
	Cores     int
	MemoryMiB int
	// DrainTimeout bounds a scale-down's decommission of this worker (S13).
	DrainTimeout time.Duration
}

// ClientRequest is what the client configuration is rendered from.
type ClientRequest struct {
	ClusterID string
	// Endpoint is the master's DNS name in the VPC, <cluster>.<vpc>.
	Endpoint string
	Secret   Secret
	// S3 is nil with object storage off; EventLogBucket is empty without an
	// event-log bucket.
	S3             *S3
	EventLogBucket string
}

// EventLogDir is a cluster's event-log location: a prefix of its own in the
// bucket, keyed by the cluster's id so a later cluster of the same name never
// shares it. The create task writes a marker object under it, because s3a
// reports a prefix with nothing under it as missing and Spark refuses to log
// into a missing directory.
func EventLogDir(bucket, clusterID string) string {
	return "s3a://" + bucket + "/spark-events/" + clusterID
}

// MasterURL is what spark-submit and the workers connect to.
func MasterURL(host string) string {
	return "spark://" + net.JoinHostPort(host, strconv.Itoa(MasterPort))
}

// ValidateMaster reports whether a master request can be rendered.
func ValidateMaster(req MasterRequest) error {
	if !idPattern.MatchString(req.ClusterID) {
		return invalid("cluster id %q", req.ClusterID)
	}
	if err := validateLabel("node name", req.NodeName); err != nil {
		return err
	}
	if _, err := parseIPv4("node ip", req.NodeIP); err != nil {
		return err
	}
	if err := validateSecret(req.Secret); err != nil {
		return err
	}
	if req.EventLog != nil {
		if err := validateS3(req.EventLog.S3); err != nil {
			return err
		}
		if err := validateBucket(req.EventLog.Bucket); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWorker reports whether a worker request can be rendered.
func ValidateWorker(req WorkerRequest) error {
	if err := validateLabel("node name", req.NodeName); err != nil {
		return err
	}
	if _, err := parseIPv4("node ip", req.NodeIP); err != nil {
		return err
	}
	if _, err := parseIPv4("master ip", req.MasterIP); err != nil {
		return err
	}
	if err := validateSecret(req.Secret); err != nil {
		return err
	}
	if req.Cores < MinWorkerCores || req.Cores > MaxWorkerCores {
		return invalid("worker cores %d", req.Cores)
	}
	if req.MemoryMiB < MinWorkerMemoryMiB {
		return invalid("worker memory %d MiB", req.MemoryMiB)
	}
	if req.DrainTimeout < MinDrainTimeout || req.DrainTimeout > MaxDrainTimeout {
		return invalid("drain timeout %s", req.DrainTimeout)
	}
	return nil
}

// ValidateClient reports whether a client configuration can be rendered.
func ValidateClient(req ClientRequest) error {
	if !idPattern.MatchString(req.ClusterID) {
		return invalid("cluster id %q", req.ClusterID)
	}
	if err := validateDNSName("endpoint", req.Endpoint); err != nil {
		return err
	}
	if err := validateSecret(req.Secret); err != nil {
		return err
	}
	if req.S3 != nil {
		if err := validateS3(*req.S3); err != nil {
			return err
		}
	}
	if req.EventLogBucket != "" {
		if req.S3 == nil {
			return invalid("an event-log bucket needs object storage")
		}
		if err := validateBucket(req.EventLogBucket); err != nil {
			return err
		}
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRequest, fmt.Sprintf(format, args...))
}

func validateLabel(field, v string) error {
	if v == "" || len(v) > MaxNameLength || !labelPattern.MatchString(v) || strings.Contains(v, "--") {
		return invalid("%s %q is not a DNS label", field, v)
	}
	return nil
}

func validateDNSName(field, v string) error {
	labels := strings.Split(v, ".")
	if len(v) > 253 || len(labels) < 2 {
		return invalid("%s %q is not a DNS name", field, v)
	}
	for _, l := range labels {
		if validateLabel(field, l) != nil {
			return invalid("%s %q is not a DNS name", field, v)
		}
	}
	return nil
}

func parseIPv4(field, v string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(v)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, invalid("%s %q is not an IPv4 address", field, v)
	}
	return addr, nil
}

func validateSecret(s Secret) error {
	if !secretPattern.MatchString(s.Value()) {
		return invalid("the Spark secret is not 64 hex characters")
	}
	return nil
}

func validateBucket(b string) error {
	if !bucketPattern.MatchString(b) || strings.Contains(b, "..") {
		return invalid("bucket %q", b)
	}
	return nil
}

// validateS3 accepts an http(s) URL of a host and an optional port, nothing
// else: no path, no query, no credentials in the URL.
func validateS3(s S3) error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return invalid("S3 endpoint %q is not an http(s) URL of a host", s.Endpoint)
	}
	host := u.Hostname()
	if _, err := parseIPv4("S3 endpoint host", host); err != nil && validateDNSName("S3 endpoint host", host) != nil &&
		validateLabel("S3 endpoint host", host) != nil {
		return invalid("S3 endpoint host %q", host)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return invalid("S3 endpoint port %q", p)
		}
	}
	if !regionPattern.MatchString(s.Region) {
		return invalid("S3 region %q", s.Region)
	}
	if !accessKeyPattern.MatchString(s.AccessKey) {
		return invalid("S3 access key id")
	}
	if !secretKeyPattern.MatchString(s.SecretKey.Value()) {
		return invalid("S3 secret key")
	}
	return nil
}

// property is one line of a spark-defaults.conf. Values are validated before
// they get here, so no escaping is needed and none is attempted.
type property struct{ key, value string }

func properties(props []property) string {
	var b strings.Builder
	for _, p := range props {
		fmt.Fprintf(&b, "%-50s %s\n", p.key, p.value)
	}
	return b.String()
}

// clusterSecurity is what every node and every client sets alike (S8, §5):
// a client without the same secret cannot reach the master at all.
func clusterSecurity(secret Secret) []property {
	return []property{
		{"spark.authenticate", "true"},
		{"spark.authenticate.secret", secret.Value()},
		{"spark.network.crypto.enabled", "true"},
	}
}

// decommissioning is a job's and a worker's: the worker handles SIGPWR with
// it, and a job's executors move their blocks off a decommissioning worker
// with the storage settings (S13).
func decommissioning() []property {
	return []property{
		{"spark.decommission.enabled", "true"},
		{"spark.storage.decommission.enabled", "true"},
		{"spark.storage.decommission.rddBlocks.enabled", "true"},
		{"spark.storage.decommission.shuffleBlocks.enabled", "true"},
	}
}

// s3Settings wire s3a to Kumo's object storage with the cluster's key.
// Path-style, because RustFS serves buckets under one host.
func s3Settings(s S3) []property {
	return []property{
		{"spark.hadoop.fs.s3a.endpoint", strings.TrimSuffix(s.Endpoint, "/")},
		{"spark.hadoop.fs.s3a.endpoint.region", s.Region},
		{"spark.hadoop.fs.s3a.path.style.access", "true"},
		{"spark.hadoop.fs.s3a.connection.ssl.enabled", strconv.FormatBool(strings.HasPrefix(s.Endpoint, "https://"))},
		{"spark.hadoop.fs.s3a.aws.credentials.provider", "org.apache.hadoop.fs.s3a.SimpleAWSCredentialsProvider"},
		{"spark.hadoop.fs.s3a.access.key", s.AccessKey},
		{"spark.hadoop.fs.s3a.secret.key", s.SecretKey.Value()},
	}
}

// daemonHardening closes what a standalone daemon would otherwise open to
// any VPC host (§5): the REST submission server, the kill links of the
// unauthenticated web UIs, and the master's decommission endpoint, which a
// scale-down does not need (S13).
func daemonHardening() []property {
	return []property{
		{"spark.master.rest.enabled", "false"},
		{"spark.ui.killEnabled", "false"},
		{"spark.master.ui.decommission.allow.mode", "DENY"},
	}
}

// ClientConfig renders the client spark-defaults.conf an admin downloads
// (09-spark.md section 5, option A). It is a Secret: it carries the Spark
// secret and, with object storage, the cluster's S3 key.
func ClientConfig(req ClientRequest) (Secret, error) {
	if err := ValidateClient(req); err != nil {
		return "", err
	}
	props := []property{{"spark.master", MasterURL(req.Endpoint)}}
	props = append(props, clusterSecurity(req.Secret)...)
	props = append(props, decommissioning()...)
	if req.S3 != nil {
		props = append(props, s3Settings(*req.S3)...)
	}
	if req.EventLogBucket != "" {
		props = append(props,
			property{"spark.eventLog.enabled", "true"},
			property{"spark.eventLog.dir", EventLogDir(req.EventLogBucket, req.ClusterID)},
		)
	}
	header := "# Kumo Spark cluster " + req.ClusterID + ": the client configuration\n" +
		"# (docs/design/09-spark.md section 5). Use it from an instance in the\n" +
		"# cluster's VPC:\n" +
		"#   spark-submit --properties-file <this file> \\\n" +
		"#     --conf spark.driver.host=$(hostname -I | cut -d' ' -f1) ...\n" +
		"# The executors connect back to the driver, and spark.driver.host makes\n" +
		"# them use this instance's address rather than a hostname they may not\n" +
		"# resolve.\n" +
		"# It carries the cluster's secret" + map[bool]string{true: " and its S3 key", false: ""}[req.S3 != nil] +
		": keep it like a password.\n"
	return Secret(header + properties(props)), nil
}

// document is the cloud-config this package renders.
type document struct {
	SSHPwauth  bool        `yaml:"ssh_pwauth"`
	Hostname   string      `yaml:"hostname"`
	WriteFiles []writeFile `yaml:"write_files"`
	Runcmd     [][]string  `yaml:"runcmd"`
}

type writeFile struct {
	Path        string `yaml:"path"`
	Owner       string `yaml:"owner,omitempty"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
}

// sparkFile is a file of /etc/spark: readable by the daemons' account and
// root only, because spark-defaults.conf carries the secret.
func sparkFile(path, content string) writeFile {
	return writeFile{Path: path, Owner: "root:spark", Permissions: "0640", Content: content}
}

// MasterUserData renders a master's seed.
func MasterUserData(req MasterRequest) (Secret, error) {
	if err := ValidateMaster(req); err != nil {
		return "", err
	}
	props := clusterSecurity(req.Secret)
	props = append(props, daemonHardening()...)
	history := "0"
	if req.EventLog != nil {
		history = "1"
		props = append(props, s3Settings(req.EventLog.S3)...)
		props = append(props,
			property{"spark.history.fs.logDirectory", EventLogDir(req.EventLog.Bucket, req.ClusterID)},
			property{"spark.history.ui.port", strconv.Itoa(HistoryPort)},
			property{"spark.history.fs.update.interval", "10s"},
			// A history server that cannot reach its bucket for a while keeps
			// serving what it has rather than exiting.
			property{"spark.history.fs.cleaner.enabled", "false"},
		)
	}
	env := "SPARK_NODE_IP=" + req.NodeIP + "\nKUMO_SPARK_HISTORY=" + history + "\n"
	return render(req.NodeName, []writeFile{
		sparkFile(defaultsPath, properties(props)),
		sparkFile(nodeEnvPath, env),
		{Path: scriptPath, Permissions: "0700", Content: masterScript},
	})
}

// WorkerUserData renders a worker's seed.
func WorkerUserData(req WorkerRequest) (Secret, error) {
	if err := ValidateWorker(req); err != nil {
		return "", err
	}
	props := clusterSecurity(req.Secret)
	props = append(props, daemonHardening()...)
	props = append(props, decommissioning()...)
	// A finished application's work directory is removed after a day, so a
	// long-lived worker's disk does not fill with old jobs' files.
	props = append(props,
		property{"spark.worker.cleanup.enabled", "true"},
		property{"spark.worker.cleanup.appDataTtl", "86400"},
	)
	env := fmt.Sprintf("SPARK_NODE_IP=%s\nSPARK_MASTER_URL=%s\nSPARK_WORKER_CORES=%d\nSPARK_WORKER_MEMORY=%dm\nSPARK_DRAIN_TIMEOUT=%d\n",
		req.NodeIP, MasterURL(req.MasterIP), req.Cores, req.MemoryMiB, int(req.DrainTimeout.Seconds()))
	return render(req.NodeName, []writeFile{
		sparkFile(defaultsPath, properties(props)),
		sparkFile(nodeEnvPath, env),
		{Path: scriptPath, Permissions: "0700", Content: workerScript},
	})
}

func render(hostname string, files []writeFile) (Secret, error) {
	body, err := yaml.Marshal(document{
		SSHPwauth:  false,
		Hostname:   hostname,
		WriteFiles: files,
		Runcmd:     [][]string{{"bash", scriptPath}},
	})
	if err != nil {
		return "", fmt.Errorf("sparkcloudinit: marshal the cloud-config: %w", err)
	}
	return Secret(cloudConfigHeader + "\n" + string(body)), nil
}
