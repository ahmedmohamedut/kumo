package sparkcloudinit_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
)

const (
	clusterID = "spk-0123456789abcdef0"
	secret    = sparkcloudinit.Secret("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	s3Secret  = sparkcloudinit.Secret("s3SecretKey0123456789abcdefGHIJ_-")
)

func s3() sparkcloudinit.S3 {
	return sparkcloudinit.S3{
		Endpoint: "http://192.168.1.10:9000", Region: "us-east-1",
		AccessKey: "KUMOABCDEFGHIJKLMNOP", SecretKey: s3Secret,
	}
}

func master() sparkcloudinit.MasterRequest {
	return sparkcloudinit.MasterRequest{
		ClusterID: clusterID, NodeName: "etl-master", NodeIP: "10.0.0.10", Secret: secret,
		EventLog: &sparkcloudinit.EventLog{S3: s3(), Bucket: "spark-logs"},
	}
}

func worker() sparkcloudinit.WorkerRequest {
	return sparkcloudinit.WorkerRequest{
		NodeName: "etl-default-1", NodeIP: "10.0.0.11", MasterIP: "10.0.0.10", Secret: secret,
		Cores: 2, MemoryMiB: 3072, DrainTimeout: 10 * time.Minute,
	}
}

func client() sparkcloudinit.ClientRequest {
	c := s3()
	return sparkcloudinit.ClientRequest{
		ClusterID: clusterID, Endpoint: "etl.default", Secret: secret, S3: &c, EventLogBucket: "spark-logs",
	}
}

type seed struct {
	SSHPwauth  bool   `yaml:"ssh_pwauth"`
	Hostname   string `yaml:"hostname"`
	WriteFiles []struct {
		Path        string `yaml:"path"`
		Owner       string `yaml:"owner"`
		Permissions string `yaml:"permissions"`
		Content     string `yaml:"content"`
	} `yaml:"write_files"`
	Runcmd [][]string `yaml:"runcmd"`
}

func parse(t *testing.T, s sparkcloudinit.Secret) (seed, map[string]string, map[string]string) {
	t.Helper()
	text := s.Value()
	if !strings.HasPrefix(text, "#cloud-config\n") {
		t.Fatalf("no cloud-config header: %.40q", text)
	}
	var doc seed
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("the seed is not YAML: %v", err)
	}
	files := map[string]string{}
	for _, f := range doc.WriteFiles {
		files[f.Path] = f.Content
		if strings.HasPrefix(f.Path, "/etc/spark/") && (f.Owner != "root:spark" || f.Permissions != "0640") {
			t.Errorf("%s is %s %s, want root:spark 0640", f.Path, f.Owner, f.Permissions)
		}
	}
	return doc, files, props(files["/etc/spark/spark-defaults.conf"])
}

func props(conf string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(conf, "\n") {
		if f := strings.Fields(line); len(f) == 2 && !strings.HasPrefix(line, "#") {
			out[f[0]] = f[1]
		}
	}
	return out
}

func TestMasterSeed(t *testing.T) {
	t.Parallel()
	out, err := sparkcloudinit.MasterUserData(master())
	if err != nil {
		t.Fatal(err)
	}
	doc, files, p := parse(t, out)
	if doc.SSHPwauth || doc.Hostname != "etl-master" || len(doc.Runcmd) != 1 || doc.Runcmd[0][0] != "bash" {
		t.Errorf("seed header = %+v", doc)
	}
	for key, want := range map[string]string{
		"spark.authenticate": "true", "spark.authenticate.secret": secret.Value(),
		"spark.network.crypto.enabled": "true", "spark.master.rest.enabled": "false",
		"spark.ui.killEnabled": "false", "spark.master.ui.decommission.allow.mode": "DENY",
		"spark.history.fs.logDirectory":         "s3a://spark-logs/spark-events/" + clusterID,
		"spark.hadoop.fs.s3a.endpoint":          "http://192.168.1.10:9000",
		"spark.hadoop.fs.s3a.path.style.access": "true", "spark.hadoop.fs.s3a.connection.ssl.enabled": "false",
		"spark.hadoop.fs.s3a.access.key": "KUMOABCDEFGHIJKLMNOP", "spark.hadoop.fs.s3a.secret.key": s3Secret.Value(),
	} {
		if p[key] != want {
			t.Errorf("%s = %q, want %q", key, p[key], want)
		}
	}
	env := files["/etc/spark/kumo-node.env"]
	if env != "SPARK_NODE_IP=10.0.0.10\nKUMO_SPARK_HISTORY=1\n" {
		t.Errorf("node env = %q", env)
	}
	// The secrets are in spark-defaults.conf and nowhere else in the seed.
	for _, s := range []string{secret.Value(), s3Secret.Value()} {
		if n := strings.Count(out.Value(), s); n != 1 {
			t.Errorf("a secret appears %d times in the seed, want once (in spark-defaults.conf)", n)
		}
	}
}

func TestMasterWithoutAnEventLogBucketRunsNoHistoryServer(t *testing.T) {
	t.Parallel()
	req := master()
	req.EventLog = nil
	out, err := sparkcloudinit.MasterUserData(req)
	if err != nil {
		t.Fatal(err)
	}
	_, files, p := parse(t, out)
	if !strings.Contains(files["/etc/spark/kumo-node.env"], "KUMO_SPARK_HISTORY=0") {
		t.Error("the history server is not off")
	}
	for key := range p {
		if strings.HasPrefix(key, "spark.hadoop.fs.s3a.") || strings.HasPrefix(key, "spark.history.") {
			t.Errorf("%s set without an event-log bucket", key)
		}
	}
}

func TestWorkerSeed(t *testing.T) {
	t.Parallel()
	out, err := sparkcloudinit.WorkerUserData(worker())
	if err != nil {
		t.Fatal(err)
	}
	doc, files, p := parse(t, out)
	if doc.Hostname != "etl-default-1" {
		t.Errorf("hostname = %q", doc.Hostname)
	}
	want := "SPARK_NODE_IP=10.0.0.11\nSPARK_MASTER_URL=spark://10.0.0.10:7077\n" +
		"SPARK_WORKER_CORES=2\nSPARK_WORKER_MEMORY=3072m\nSPARK_DRAIN_TIMEOUT=600\n"
	if env := files["/etc/spark/kumo-node.env"]; env != want {
		t.Errorf("node env = %q\nwant       %q", env, want)
	}
	if p["spark.decommission.enabled"] != "true" || p["spark.authenticate.secret"] != secret.Value() {
		t.Errorf("worker defaults = %v", p)
	}
	// No S3 credential on a worker: a job's reach its executors from its
	// driver (section 5).
	for key := range p {
		if strings.HasPrefix(key, "spark.hadoop.") {
			t.Errorf("%s set on a worker", key)
		}
	}
}

func TestClientConfig(t *testing.T) {
	t.Parallel()
	out, err := sparkcloudinit.ClientConfig(client())
	if err != nil {
		t.Fatal(err)
	}
	p := props(out.Value())
	for key, want := range map[string]string{
		"spark.master": "spark://etl.default:7077", "spark.authenticate.secret": secret.Value(),
		"spark.network.crypto.enabled": "true", "spark.decommission.enabled": "true",
		"spark.hadoop.fs.s3a.secret.key": s3Secret.Value(),
		"spark.eventLog.enabled":         "true", "spark.eventLog.dir": "s3a://spark-logs/spark-events/" + clusterID,
	} {
		if p[key] != want {
			t.Errorf("%s = %q, want %q", key, p[key], want)
		}
	}
	if !strings.Contains(out.Value(), "keep it like a password") {
		t.Error("the file does not say it carries secrets")
	}

	// Without object storage: no S3 settings and no event log.
	req := client()
	req.S3, req.EventLogBucket = nil, ""
	out, err = sparkcloudinit.ClientConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	for key := range props(out.Value()) {
		if strings.HasPrefix(key, "spark.hadoop.") || strings.HasPrefix(key, "spark.eventLog.") {
			t.Errorf("%s set with object storage off", key)
		}
	}
}

func TestHTTPSEndpointTurnsSSLOn(t *testing.T) {
	t.Parallel()
	req := client()
	req.S3.Endpoint = "https://s3.example.internal"
	out, err := sparkcloudinit.ClientConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if v := props(out.Value())["spark.hadoop.fs.s3a.connection.ssl.enabled"]; v != "true" {
		t.Errorf("ssl.enabled = %q for an https endpoint", v)
	}
}

// Every value that reaches a file is checked, so nothing a request carries
// can add a line to spark-defaults.conf or kumo-node.env.
func TestRefusals(t *testing.T) {
	t.Parallel()
	hostile := "a\nspark.authenticate false"
	masters := map[string]func(*sparkcloudinit.MasterRequest){
		"bad cluster id":       func(r *sparkcloudinit.MasterRequest) { r.ClusterID = "eks-0123456789abcdef0" },
		"newline in node name": func(r *sparkcloudinit.MasterRequest) { r.NodeName = hostile },
		"uppercase node name":  func(r *sparkcloudinit.MasterRequest) { r.NodeName = "Master" },
		"IPv6 node ip":         func(r *sparkcloudinit.MasterRequest) { r.NodeIP = "fd00::1" },
		"short secret":         func(r *sparkcloudinit.MasterRequest) { r.Secret = "abc" },
		"secret with newline":  func(r *sparkcloudinit.MasterRequest) { r.Secret = sparkcloudinit.Secret(hostile) },
		"bucket with space":    func(r *sparkcloudinit.MasterRequest) { r.EventLog.Bucket = "my bucket" },
		"bucket with dots":     func(r *sparkcloudinit.MasterRequest) { r.EventLog.Bucket = "a..b" },
		"endpoint with path":   func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Endpoint = "http://h:9000/x" },
		"endpoint with user":   func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Endpoint = "http://u:p@h:9000" },
		"endpoint ftp":         func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Endpoint = "ftp://h" },
		"endpoint newline":     func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Endpoint = "http://h:9000\nx" },
		"endpoint bad port":    func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Endpoint = "http://h:70000" },
		"access key space":     func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.AccessKey = "KUMO ABCDEFGHIJKLMNOP" },
		"secret key newline": func(r *sparkcloudinit.MasterRequest) {
			r.EventLog.S3.SecretKey = sparkcloudinit.Secret(hostile + "0123456789")
		},
		"region uppercase": func(r *sparkcloudinit.MasterRequest) { r.EventLog.S3.Region = "US-EAST-1" },
	}
	for name, mutate := range masters {
		req := master()
		el := *req.EventLog
		req.EventLog = &el
		mutate(&req)
		if _, err := sparkcloudinit.MasterUserData(req); !errors.Is(err, sparkcloudinit.ErrInvalidRequest) {
			t.Errorf("master, %s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	workers := map[string]func(*sparkcloudinit.WorkerRequest){
		"bad master ip":     func(r *sparkcloudinit.WorkerRequest) { r.MasterIP = "master" },
		"no cores":          func(r *sparkcloudinit.WorkerRequest) { r.Cores = 0 },
		"too little memory": func(r *sparkcloudinit.WorkerRequest) { r.MemoryMiB = 256 },
		"drain too short":   func(r *sparkcloudinit.WorkerRequest) { r.DrainTimeout = time.Second },
		"drain too long":    func(r *sparkcloudinit.WorkerRequest) { r.DrainTimeout = 3 * time.Hour },
	}
	for name, mutate := range workers {
		req := worker()
		mutate(&req)
		if _, err := sparkcloudinit.WorkerUserData(req); !errors.Is(err, sparkcloudinit.ErrInvalidRequest) {
			t.Errorf("worker, %s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
	clients := map[string]func(*sparkcloudinit.ClientRequest){
		"one-label endpoint":               func(r *sparkcloudinit.ClientRequest) { r.Endpoint = "etl" },
		"endpoint newline":                 func(r *sparkcloudinit.ClientRequest) { r.Endpoint = "etl.default\nx" },
		"event log without object storage": func(r *sparkcloudinit.ClientRequest) { r.S3 = nil },
	}
	for name, mutate := range clients {
		req := client()
		mutate(&req)
		if _, err := sparkcloudinit.ClientConfig(req); !errors.Is(err, sparkcloudinit.ErrInvalidRequest) {
			t.Errorf("client, %s: err = %v, want ErrInvalidRequest", name, err)
		}
	}
}

func TestSecretNeverPrints(t *testing.T) {
	t.Parallel()
	holder := struct{ S sparkcloudinit.Secret }{secret}
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		if out := fmt.Sprintf(format, holder); strings.Contains(out, secret.Value()) || strings.Contains(out, "0123456789abcdef") {
			t.Errorf("%s printed the secret: %s", format, out)
		}
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).InfoContext(t.Context(), "x", "secret", secret)
	if strings.Contains(buf.String(), secret.Value()) {
		t.Error("slog printed the secret")
	}
	if _, err := json.Marshal(holder); !errors.Is(err, sparkcloudinit.ErrSecretMarshal) {
		t.Errorf("json.Marshal() = %v, want ErrSecretMarshal", err)
	}
}

type fixedReader struct{ b byte }

func (r fixedReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

func TestNewSecret(t *testing.T) {
	t.Parallel()
	s, err := sparkcloudinit.NewSecret(fixedReader{0xab})
	if err != nil || s.Value() != strings.Repeat("ab", 32) {
		t.Errorf("NewSecret() = %q, %v", s.Value(), err)
	}
	a, _ := sparkcloudinit.NewSecret(nil)
	b, _ := sparkcloudinit.NewSecret(nil)
	if len(a.Value()) != 64 || a.Value() == b.Value() {
		t.Error("two random secrets are not distinct 64-character strings")
	}
	// What NewSecret makes, every renderer accepts.
	req := worker()
	req.Secret = a
	if _, err := sparkcloudinit.WorkerUserData(req); err != nil {
		t.Errorf("a generated secret is refused: %v", err)
	}
}

func TestScriptsAreYAMLFriendly(t *testing.T) {
	t.Parallel()
	for name, render := range map[string]func() (sparkcloudinit.Secret, error){
		"master": func() (sparkcloudinit.Secret, error) { return sparkcloudinit.MasterUserData(master()) },
		"worker": func() (sparkcloudinit.Secret, error) { return sparkcloudinit.WorkerUserData(worker()) },
	} {
		out, err := render()
		if err != nil {
			t.Fatal(err)
		}
		_, files, _ := parse(t, out)
		script := files["/run/kumo-spark/firstboot.sh"]
		if !strings.HasPrefix(script, "#!/usr/bin/env bash\n") || strings.Contains(script, "\t") {
			t.Errorf("%s script is not a tab-free bash script", name)
		}
		if !strings.Contains(script, "shred -u") {
			t.Errorf("%s script does not shred the seed", name)
		}
	}
}
