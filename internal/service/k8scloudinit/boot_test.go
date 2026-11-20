//go:build kubeimage

// The M8.2 acceptance run: the seeds this package renders, with a PKI from
// internal/service/k8spki, boot the M8.1 image (packaging/images/kubernetes-1.37)
// as a control plane and a worker that joins it, and the cluster is checked
// through its real API server (docs/design/08-kubernetes.md sections 4 to 6).
//
//	KUMO_TEST_K8S_IMAGE=/path/to/kumo-kubernetes-1.37-<date>.qcow2 make test-k8s-image
//
// KUMO_TEST_DATA_DIR puts the VMs' disks somewhere other than $TMPDIR, which
// matters where /tmp is a tmpfs: each node writes a few GiB. It needs qemu,
// qemu-img, xorriso and a writable /dev/kvm, and changes nothing on the host.
package k8scloudinit_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/k8scloudinit"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
)

const (
	bootEndpoint = "api.boot.kumo"
	cpIP         = "10.250.0.10"
	workerIP     = "10.250.0.11"
	cpMAC        = "52:54:00:4b:01:10"
	workerMAC    = "52:54:00:4b:01:11"
	// hostMAC is the control plane's second NIC: QEMU's user network with
	// restrict=on, which forwards the API port from the host and routes
	// nothing out, so the nodes still have no way to a registry.
	hostMAC = "52:54:00:4b:01:20"
)

func TestSeedsBootAClusterOnTheImage(t *testing.T) {
	image := os.Getenv("KUMO_TEST_K8S_IMAGE")
	if image == "" {
		t.Skip("set KUMO_TEST_K8S_IMAGE to the qcow2 from packaging/images/kubernetes-1.37 to run this")
	}
	for _, tool := range []string{"qemu-system-x86_64", "qemu-img", "xorriso"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	work := t.TempDir()
	if dir := os.Getenv("KUMO_TEST_DATA_DIR"); dir != "" {
		var err error
		if work, err = os.MkdirTemp(dir, "k8s-boot-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(work) })
	}

	// What kumod does before any VM boots (section 4): the PKI, the token and
	// the CA hash, then the two seeds.
	now := time.Now().UTC()
	pki, err := k8spki.Generate("boot", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := k8spki.NewBootstrapToken(nil)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := k8spki.CAHash(pki.CA.CertPEM)
	if err != nil {
		t.Fatal(err)
	}
	cpSeed, err := k8scloudinit.ControlPlaneUserData(k8scloudinit.ControlPlaneRequest{
		ClusterName: "boot", NodeName: "cp1", Endpoint: bootEndpoint, NodeIP: cpIP,
		PodCIDR: "100.64.0.0/16", ServiceCIDR: "100.96.0.0/16", Token: token, PKI: pki,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerSeed, err := k8scloudinit.WorkerUserData(k8scloudinit.WorkerRequest{
		NodeName: "w1", Endpoint: bootEndpoint, ControlPlaneIP: cpIP, NodeIP: workerIP,
		Token: token, CAHash: hash,
	})
	if err != nil {
		t.Fatal(err)
	}

	link := freePort(t)
	apiPort := freePort(t)
	cpNet := fmt.Sprintf(`version: 2
ethernets:
  kumo0:
    match: {macaddress: "%s"}
    set-name: kumo0
    addresses: [%s/24]
    routes: [{to: default, via: 10.250.0.1}]
    nameservers: {addresses: [10.250.0.1]}
  host0:
    match: {macaddress: "%s"}
    set-name: host0
    addresses: [10.0.2.15/24]
`, cpMAC, cpIP, hostMAC)
	workerNet := fmt.Sprintf(`version: 2
ethernets:
  kumo0:
    match: {macaddress: "%s"}
    set-name: kumo0
    addresses: [%s/24]
    routes: [{to: default, via: 10.250.0.1}]
    nameservers: {addresses: [10.250.0.1]}
`, workerMAC, workerIP)

	cp := bootNode(t, work, image, "cp1", 3072, cpSeed, cpNet, []string{
		"-netdev", fmt.Sprintf("socket,id=k0,listen=127.0.0.1:%d", link),
		"-device", "virtio-net-pci,netdev=k0,mac=" + cpMAC,
		"-netdev", fmt.Sprintf("user,id=h0,restrict=on,hostfwd=tcp:127.0.0.1:%d-:6443", apiPort),
		"-device", "virtio-net-pci,netdev=h0,mac=" + hostMAC,
	})
	time.Sleep(2 * time.Second)
	worker := bootNode(t, work, image, "w1", 2048, workerSeed, workerNet, []string{
		"-netdev", fmt.Sprintf("socket,id=k0,connect=127.0.0.1:%d", link),
		"-device", "virtio-net-pci,netdev=k0,mac=" + workerMAC,
	})

	base := fmt.Sprintf("https://127.0.0.1:%d", apiPort)
	// kumod's own identity, trusting only the cluster CA: every answer below
	// proves the API server's certificate was signed with kumod's key.
	system := client(t, pki, pki.System.CertPEM, pki.System.KeyPEM.Bytes())

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Minute)
	defer cancel()
	waitFor(ctx, t, "the API server to be ready", []*node{cp, worker}, func() bool {
		status, body := call(system, http.MethodGet, base+"/readyz", "")
		return status == http.StatusOK && strings.TrimSpace(body) == "ok"
	})
	t.Logf("control plane ready after %s", time.Since(now).Round(time.Second))

	waitFor(ctx, t, "both nodes to be Ready", []*node{cp, worker}, func() bool {
		return readyNodes(system, base) == 2
	})
	t.Logf("both nodes Ready after %s", time.Since(now).Round(time.Second))

	// kumod's role is exactly section 6: nodes yes, anything else no.
	expect(t, system, http.MethodGet, base+"/api/v1/nodes", "", http.StatusOK, "kumo:system reads nodes")
	expect(t, system, http.MethodGet, base+"/api/v1/namespaces/kube-system/secrets", "", http.StatusForbidden,
		"kumo:system must not read secrets")
	expect(t, system, http.MethodPost, base+"/api/v1/namespaces/default/configmaps", configMap("by-kumod"),
		http.StatusForbidden, "kumo:system must not write workloads")

	// Kubeconfigs as a user downloads them, minted from the CA (K10).
	admin := kubeconfigClient(t, pki, k8spki.GroupAdmins, base)
	viewer := kubeconfigClient(t, pki, k8spki.GroupViewers, base)
	expect(t, admin, http.MethodPost, base+"/api/v1/namespaces/default/configmaps", configMap("by-admin"),
		http.StatusCreated, "an admin kubeconfig writes")
	expect(t, viewer, http.MethodGet, base+"/api/v1/namespaces/default/configmaps", "", http.StatusOK,
		"a viewer kubeconfig reads")
	expect(t, viewer, http.MethodPost, base+"/api/v1/namespaces/default/configmaps", configMap("by-viewer"),
		http.StatusForbidden, "a viewer kubeconfig must not write")

	// No key and no token secret on either console.
	for _, n := range []*node{cp, worker} {
		log, _ := os.ReadFile(n.serial)
		if strings.Contains(string(log), "PRIVATE KEY") {
			t.Errorf("%s's console printed a private key", n.name)
		}
		if bytes.Contains(log, token.Secret.Bytes()) {
			t.Errorf("%s's console printed the bootstrap token's secret", n.name)
		}
	}
}

type node struct {
	name   string
	serial string
}

func bootNode(t *testing.T, work, image, name string, memMiB int, userData, network string, nics []string) *node {
	t.Helper()

	seedDir := filepath.Join(work, name+"-seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"user-data":      userData,
		"meta-data":      fmt.Sprintf("instance-id: kumo-boot-%s-%d\nlocal-hostname: %s\n", name, time.Now().UnixNano(), name),
		"network-config": network,
	}
	for f, content := range files {
		if err := os.WriteFile(filepath.Join(seedDir, f), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	iso := filepath.Join(work, name+"-seed.iso")
	run(t, "xorriso", "-as", "mkisofs", "-quiet", "-V", "cidata", "-J", "-r", "-o", iso, seedDir)
	root := filepath.Join(work, name+"-root.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", image, root, "20G")

	n := &node{name: name, serial: filepath.Join(work, name+"-serial.log")}
	args := []string{
		"-enable-kvm", "-cpu", "host", "-m", strconv.Itoa(memMiB), "-smp", "2",
		"-display", "none", "-monitor", "none", "-no-reboot",
		"-serial", "file:" + n.serial,
		"-drive", "if=none,id=root,file=" + root + ",format=qcow2",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		"-drive", "if=none,id=seed,file=" + iso + ",format=raw,readonly=on",
		"-device", "virtio-blk-pci,drive=seed",
	}
	cmd := exec.Command("qemu-system-x86_64", append(args, nics...)...)
	stderr, _ := os.Create(filepath.Join(work, name+"-qemu.stderr"))
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			// The tail of each console is the only account of what a node did.
			log, _ := os.ReadFile(n.serial)
			lines := strings.Split(string(log), "\n")
			t.Logf("--- %s console (last 40 lines)\n%s", name, strings.Join(lines[max(0, len(lines)-40):], "\n"))
		}
	})
	return n
}

// waitFor polls cond every 10 s, failing early when a node's first boot
// reports a failure.
func waitFor(ctx context.Context, t *testing.T, what string, nodes []*node, cond func() bool) {
	t.Helper()
	for {
		if cond() {
			return
		}
		for _, n := range nodes {
			if log, _ := os.ReadFile(n.serial); strings.Contains(string(log), "KUMO-K8S-FIRSTBOOT-FAILED") {
				t.Fatalf("%s reported a failed first boot while waiting for %s", n.name, what)
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(10 * time.Second):
		}
	}
}

func client(t *testing.T, pki *k8spki.ClusterPKI, certPEM, keyPEM []byte) *http.Client {
	t.Helper()
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("client certificate: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pki.CA.CertPEM) {
		t.Fatal("cannot add the cluster CA")
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      roots,
			Certificates: []tls.Certificate{pair},
			// The API server's certificate names the endpoint, not the
			// forwarded loopback address the test dials.
			ServerName: bootEndpoint,
			MinVersion: tls.VersionTLS12,
		}},
	}
}

// kubeconfigClient mints a kubeconfig the way a download does and builds a
// client from the file itself, so the file's format is what is tested.
func kubeconfigClient(t *testing.T, pki *k8spki.ClusterPKI, group, base string) *http.Client {
	t.Helper()
	raw, err := pki.MintKubeconfig(k8spki.KubeconfigRequest{
		ClusterName: "boot", Server: "https://" + bootEndpoint + ":6443", User: "tester-" + strings.TrimPrefix(group, "kumo:"),
		Group: group, Now: time.Now().UTC(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Users []struct {
			User struct {
				Cert string `yaml:"client-certificate-data"`
				Key  string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(raw.Bytes(), &doc); err != nil || len(doc.Users) != 1 {
		t.Fatalf("kubeconfig: %v", err)
	}
	cert, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Cert)
	key, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Key)
	return client(t, pki, cert, key)
}

func call(c *http.Client, method, url, body string) (int, string) {
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(b)
}

func expect(t *testing.T, c *http.Client, method, url, body string, want int, what string) {
	t.Helper()
	if status, resp := call(c, method, url, body); status != want {
		t.Errorf("%s: %s %s = %d, want %d (%s)", what, method, url, status, want, strings.TrimSpace(resp)[:min(200, len(strings.TrimSpace(resp)))])
	}
}

func readyNodes(c *http.Client, base string) int {
	status, body := call(c, http.MethodGet, base+"/api/v1/nodes", "")
	if status != http.StatusOK {
		return 0
	}
	var list struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal([]byte(body), &list) != nil {
		return 0
	}
	ready := 0
	for _, item := range list.Items {
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready++
			}
		}
	}
	return ready
}

func configMap(name string) string {
	return fmt.Sprintf(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":%q},"data":{"k":"v"}}`, name)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", l.Addr())
	}
	return addr.Port
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}
