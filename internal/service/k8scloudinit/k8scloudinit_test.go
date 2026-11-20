package k8scloudinit_test

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/k8scloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
)

type file struct {
	Path        string `yaml:"path"`
	Permissions string `yaml:"permissions"`
	Content     string `yaml:"content"`
	Append      bool   `yaml:"append"`
}

type seed struct {
	SSHPwauth      bool       `yaml:"ssh_pwauth"`
	Hostname       string     `yaml:"hostname"`
	ManageEtcHosts bool       `yaml:"manage_etc_hosts"`
	WriteFiles     []file     `yaml:"write_files"`
	Runcmd         [][]string `yaml:"runcmd"`
}

func (s seed) file(t *testing.T, path string) file {
	t.Helper()
	for _, f := range s.WriteFiles {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("the seed writes no %s", path)
	return file{}
}

func parseSeed(t *testing.T, userData string) seed {
	t.Helper()
	if !strings.HasPrefix(userData, "#cloud-config\n") {
		t.Fatalf("user-data does not start with #cloud-config: %q", userData[:min(40, len(userData))])
	}
	var s seed
	if err := yaml.Unmarshal([]byte(userData), &s); err != nil {
		t.Fatalf("user-data is not YAML: %v", err)
	}
	return s
}

// docs splits a multi-document YAML file into generic maps.
func docs(t *testing.T, content string) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(content))
	var out []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("not YAML: %v", err)
		}
		out = append(out, m)
	}
}

func dig(m map[string]any, keys ...string) any {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func fixture(t *testing.T) (k8scloudinit.ControlPlaneRequest, k8scloudinit.WorkerRequest) {
	t.Helper()
	pki, err := k8spki.Generate("orders", time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := k8spki.NewBootstrapToken(nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := k8spki.CAHash(pki.CA.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	cp := k8scloudinit.ControlPlaneRequest{
		ClusterName: "orders", NodeName: "orders-cp-1", Endpoint: "api.orders.default",
		NodeIP: "192.168.100.10", PodCIDR: "100.64.0.0/16", ServiceCIDR: "100.96.0.0/16",
		Token: tok, PKI: pki,
	}
	w := k8scloudinit.WorkerRequest{
		NodeName: "orders-default-1", Endpoint: "api.orders.default",
		ControlPlaneIP: "192.168.100.10", NodeIP: "192.168.100.11", Token: tok, CAHash: hash,
	}
	return cp, w
}

func TestControlPlaneUserData(t *testing.T) {
	t.Parallel()

	req, _ := fixture(t)
	out, err := k8scloudinit.ControlPlaneUserData(req)
	if err != nil {
		t.Fatalf("ControlPlaneUserData(): %v", err)
	}
	s := parseSeed(t, out)

	if s.SSHPwauth || s.ManageEtcHosts {
		t.Errorf("ssh_pwauth = %v, manage_etc_hosts = %v, want both false", s.SSHPwauth, s.ManageEtcHosts)
	}
	if s.Hostname != "orders-cp-1" {
		t.Errorf("hostname = %q", s.Hostname)
	}
	if len(s.Runcmd) != 1 || strings.Join(s.Runcmd[0], " ") != "bash /run/kumo-k8s/firstboot.sh" {
		t.Errorf("runcmd = %v, want bash <script>: /run is noexec", s.Runcmd)
	}

	// The PKI, where kubeadm looks for it, keys 0600 and certificates 0644,
	// byte for byte what kumod generated.
	p := req.PKI
	for path, want := range map[string]struct {
		perm    string
		content []byte
	}{
		"/etc/kubernetes/pki/ca.crt":             {"0644", p.CA.CertPEM},
		"/etc/kubernetes/pki/ca.key":             {"0600", p.CA.KeyPEM.Bytes()},
		"/etc/kubernetes/pki/front-proxy-ca.crt": {"0644", p.FrontProxyCA.CertPEM},
		"/etc/kubernetes/pki/front-proxy-ca.key": {"0600", p.FrontProxyCA.KeyPEM.Bytes()},
		"/etc/kubernetes/pki/etcd/ca.crt":        {"0644", p.EtcdCA.CertPEM},
		"/etc/kubernetes/pki/etcd/ca.key":        {"0600", p.EtcdCA.KeyPEM.Bytes()},
		"/etc/kubernetes/pki/sa.key":             {"0600", p.ServiceAccountKey.Bytes()},
		"/etc/kubernetes/pki/sa.pub":             {"0644", p.ServiceAccountPub},
	} {
		f := s.file(t, path)
		if f.Permissions != want.perm {
			t.Errorf("%s permissions = %s, want %s", path, f.Permissions, want.perm)
		}
		if f.Content != string(want.content) {
			t.Errorf("%s does not hold kumod's material", path)
		}
	}
	// kumod's own client key is kumod's: it never goes to a node.
	if strings.Contains(out, string(p.System.KeyPEM.Bytes())) {
		t.Error("the seed carries kumod's client key")
	}

	hosts := s.file(t, "/etc/hosts")
	if !hosts.Append || hosts.Content != "192.168.100.10 api.orders.default\n" {
		t.Errorf("/etc/hosts = %+v, want the endpoint appended at the node's address", hosts)
	}

	config := docs(t, s.file(t, "/run/kumo-k8s/kubeadm.yaml").Content)
	if len(config) != 2 {
		t.Fatalf("the kubeadm configuration has %d documents, want InitConfiguration and ClusterConfiguration", len(config))
	}
	init, cluster := config[0], config[1]
	for _, d := range config {
		if d["apiVersion"] != "kubeadm.k8s.io/v1beta4" {
			t.Errorf("apiVersion = %v, want v1beta4", d["apiVersion"])
		}
	}
	if init["kind"] != "InitConfiguration" || cluster["kind"] != "ClusterConfiguration" {
		t.Fatalf("kinds = %v, %v", init["kind"], cluster["kind"])
	}
	tokens, _ := init["bootstrapTokens"].([]any)
	first, _ := tokens[0].(map[string]any)
	if len(tokens) != 1 || dig(first, "token") != req.Token.Value() {
		t.Errorf("bootstrapTokens = %v, want the request's token", tokens)
	}
	if got := dig(init, "localAPIEndpoint", "advertiseAddress"); got != "192.168.100.10" {
		t.Errorf("advertiseAddress = %v", got)
	}
	if got := dig(init, "nodeRegistration", "criSocket"); got != "unix:///var/run/crio/crio.sock" {
		t.Errorf("criSocket = %v, want CRI-O's", got)
	}
	for key, want := range map[string]any{
		"clusterName": "orders",
		// Filled in at boot from the node's own kubeadm, so a seed never names
		// a version the image does not carry.
		"kubernetesVersion":    "@KUBERNETES_VERSION@",
		"controlPlaneEndpoint": "api.orders.default:6443",
		"encryptionAlgorithm":  "ECDSA-P256",
	} {
		if cluster[key] != want {
			t.Errorf("%s = %v, want %v", key, cluster[key], want)
		}
	}
	if got := dig(cluster, "networking", "podSubnet"); got != "100.64.0.0/16" {
		t.Errorf("podSubnet = %v", got)
	}
	if got := dig(cluster, "networking", "serviceSubnet"); got != "100.96.0.0/16" {
		t.Errorf("serviceSubnet = %v", got)
	}
	if sans, _ := dig(cluster, "apiServer", "certSANs").([]any); len(sans) != 1 || sans[0] != "api.orders.default" {
		t.Errorf("certSANs = %v, want the endpoint", sans)
	}
	if f := s.file(t, "/run/kumo-k8s/kubeadm.yaml"); f.Permissions != "0600" {
		t.Errorf("the kubeadm configuration holds the token and is %s, want 0600", f.Permissions)
	}

	assertScript(t, s.file(t, "/run/kumo-k8s/firstboot.sh").Content, "kubeadm init")
	if !strings.Contains(out, "#!/usr/bin/env bash\n") {
		t.Error("the script is not a readable literal block in the seed")
	}
}

// Kumo's groups and kumod's own role, exactly as section 6 and K10 say.
func TestRBAC(t *testing.T) {
	t.Parallel()

	req, _ := fixture(t)
	out, _ := k8scloudinit.ControlPlaneUserData(req)
	rbac := docs(t, parseSeed(t, out).file(t, "/run/kumo-k8s/rbac.yaml").Content)

	bindings := map[string]string{}
	var systemRules, viewerNodeRules []any
	for _, d := range rbac {
		name, _ := dig(d, "metadata", "name").(string)
		switch d["kind"] {
		case "ClusterRoleBinding", "RoleBinding":
			bindings[name], _ = dig(d, "roleRef", "name").(string)
		case "ClusterRole":
			switch name {
			case "kumo:system":
				systemRules, _ = d["rules"].([]any)
			case "kumo:viewers-nodes":
				viewerNodeRules, _ = d["rules"].([]any)
			}
		}
	}
	for binding, role := range map[string]string{
		"kumo:admins":                  "cluster-admin",
		"kumo:viewers":                 "view",
		"kumo:viewers-nodes":           "kumo:viewers-nodes",
		"kumo:system":                  "kumo:system",
		"kumo:system-bootstrap-tokens": "kumo:system-bootstrap-tokens",
	} {
		if bindings[binding] != role {
			t.Errorf("binding %s -> %q, want %q", binding, bindings[binding], role)
		}
	}
	if strings.Contains(out, "system:masters") {
		t.Error("the seed mentions system:masters")
	}

	allowed := map[string]bool{
		"nodes/get": true, "nodes/list": true, "nodes/watch": true, "nodes/patch": true, "nodes/delete": true,
		"pods/get": true, "pods/list": true, "pods/eviction/create": true,
	}
	for _, r := range systemRules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		for _, res := range resources {
			for _, v := range verbs {
				resource, _ := res.(string)
				verb, _ := v.(string)
				if key := resource + "/" + verb; !allowed[key] {
					t.Errorf("kumo:system may %s, which section 6 does not need", key)
				}
			}
		}
	}

	// A viewer reads nodes and does nothing else to them.
	var viewerGrants []string
	for _, r := range viewerNodeRules {
		rule, _ := r.(map[string]any)
		resources, _ := rule["resources"].([]any)
		verbs, _ := rule["verbs"].([]any)
		for _, res := range resources {
			for _, v := range verbs {
				resource, _ := res.(string)
				verb, _ := v.(string)
				viewerGrants = append(viewerGrants, resource+"/"+verb)
			}
		}
	}
	if got := strings.Join(viewerGrants, " "); got != "nodes/get nodes/list nodes/watch" {
		t.Errorf("kumo:viewers-nodes grants %q, want read-only on nodes", got)
	}
}

func TestWorkerUserData(t *testing.T) {
	t.Parallel()

	_, req := fixture(t)
	out, err := k8scloudinit.WorkerUserData(req)
	if err != nil {
		t.Fatalf("WorkerUserData(): %v", err)
	}
	s := parseSeed(t, out)

	// A worker's seed holds an expiring token and a hash, never a key.
	if strings.Contains(out, "PRIVATE KEY") {
		t.Error("a worker's seed carries a private key")
	}
	if s.Hostname != "orders-default-1" {
		t.Errorf("hostname = %q", s.Hostname)
	}
	hosts := s.file(t, "/etc/hosts")
	if hosts.Content != "192.168.100.10 api.orders.default\n" {
		t.Errorf("/etc/hosts = %q, want the endpoint at the control plane", hosts.Content)
	}

	config := docs(t, s.file(t, "/run/kumo-k8s/kubeadm.yaml").Content)
	if len(config) != 1 || config[0]["kind"] != "JoinConfiguration" {
		t.Fatalf("config = %v, want one JoinConfiguration", config)
	}
	join := config[0]
	if got := dig(join, "discovery", "bootstrapToken", "apiServerEndpoint"); got != "api.orders.default:6443" {
		t.Errorf("apiServerEndpoint = %v", got)
	}
	if got := dig(join, "discovery", "bootstrapToken", "token"); got != req.Token.Value() {
		t.Error("the join does not carry the request's token")
	}
	// Pinned: never unsafeSkipCAVerification.
	hashes, _ := dig(join, "discovery", "bootstrapToken", "caCertHashes").([]any)
	if len(hashes) != 1 || hashes[0] != req.CAHash {
		t.Errorf("caCertHashes = %v, want the CA's hash", hashes)
	}
	if strings.Contains(out, "unsafeSkipCAVerification") {
		t.Error("the join skips CA verification")
	}
	args, _ := dig(join, "nodeRegistration", "kubeletExtraArgs").([]any)
	firstArg, _ := args[0].(map[string]any)
	if len(args) != 1 || dig(firstArg, "value") != "192.168.100.11" {
		t.Errorf("kubeletExtraArgs = %v, want node-ip", args)
	}

	assertScript(t, s.file(t, "/run/kumo-k8s/firstboot.sh").Content, "kubeadm join")
}

// assertScript runs the script through bash -n and checks the guards that make
// a re-run safe and the cleanup that removes the token.
func assertScript(t *testing.T, script, step string) {
	t.Helper()

	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the script is not valid bash: %v\n%s", err, stderr.String())
	}
	if strings.Contains(script, "\t") {
		t.Error("the script contains a tab, which would make it an escaped YAML string")
	}
	for _, want := range []string{
		"set -euo pipefail", step,
		// Idempotence: a node that is already initialized or joined skips
		// kubeadm rather than failing on it.
		"if [ ! -f /etc/kubernetes/",
		// The token is removed with the configuration, and cloud-init's copies
		// of the seed are shredded.
		`rm -f "$RUN/kubeadm.yaml"`, "shred -u",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script does not contain %q", want)
		}
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()

	cp, w := fixture(t)

	cpCases := map[string]func(*k8scloudinit.ControlPlaneRequest){
		"an uppercase cluster name":      func(r *k8scloudinit.ControlPlaneRequest) { r.ClusterName = "Orders" },
		"a cluster name with a newline":  func(r *k8scloudinit.ControlPlaneRequest) { r.ClusterName = "orders\nx" },
		"a cluster name ending in -":     func(r *k8scloudinit.ControlPlaneRequest) { r.ClusterName = "orders-" },
		"a node name with a quote":       func(r *k8scloudinit.ControlPlaneRequest) { r.NodeName = "cp'1" },
		"a one-label endpoint":           func(r *k8scloudinit.ControlPlaneRequest) { r.Endpoint = "api" },
		"an endpoint with a space":       func(r *k8scloudinit.ControlPlaneRequest) { r.Endpoint = "api.orders .default" },
		"an IPv6 node address":           func(r *k8scloudinit.ControlPlaneRequest) { r.NodeIP = "fd00::1" },
		"a node address that is no IP":   func(r *k8scloudinit.ControlPlaneRequest) { r.NodeIP = "cp1" },
		"an unmasked pod range":          func(r *k8scloudinit.ControlPlaneRequest) { r.PodCIDR = "100.64.0.1/16" },
		"overlapping ranges":             func(r *k8scloudinit.ControlPlaneRequest) { r.ServiceCIDR = "100.64.128.0/17" },
		"a pod range holding the node":   func(r *k8scloudinit.ControlPlaneRequest) { r.PodCIDR = "192.168.0.0/16" },
		"a malformed token":              func(r *k8scloudinit.ControlPlaneRequest) { r.Token = k8spki.BootstrapToken{ID: "x"} },
		"no PKI":                         func(r *k8scloudinit.ControlPlaneRequest) { r.PKI = nil },
		"a PKI whose CA key is the etcd": func(r *k8scloudinit.ControlPlaneRequest) { p := *r.PKI; p.CA.KeyPEM = p.EtcdCA.KeyPEM; r.PKI = &p },
	}
	for name, mutate := range cpCases {
		t.Run("control plane: "+name, func(t *testing.T) {
			t.Parallel()
			req := cp
			mutate(&req)
			if _, err := k8scloudinit.ControlPlaneUserData(req); !errors.Is(err, k8scloudinit.ErrInvalidRequest) {
				t.Errorf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}

	wCases := map[string]func(*k8scloudinit.WorkerRequest){
		"a node name with a slash":      func(r *k8scloudinit.WorkerRequest) { r.NodeName = "w/1" },
		"a control plane that is no IP": func(r *k8scloudinit.WorkerRequest) { r.ControlPlaneIP = "api" },
		"a hash without its prefix":     func(r *k8scloudinit.WorkerRequest) { r.CAHash = strings.TrimPrefix(r.CAHash, "sha256:") },
		"a short hash":                  func(r *k8scloudinit.WorkerRequest) { r.CAHash = "sha256:abc" },
		"a malformed token":             func(r *k8scloudinit.WorkerRequest) { r.Token = k8spki.BootstrapToken{} },
	}
	for name, mutate := range wCases {
		t.Run("worker: "+name, func(t *testing.T) {
			t.Parallel()
			req := w
			mutate(&req)
			if _, err := k8scloudinit.WorkerUserData(req); !errors.Is(err, k8scloudinit.ErrInvalidRequest) {
				t.Errorf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
