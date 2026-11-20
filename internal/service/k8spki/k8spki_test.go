package k8spki_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
)

var issued = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

func generate(t *testing.T) *k8spki.ClusterPKI {
	t.Helper()
	p, err := k8spki.Generate("orders", issued, nil)
	if err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	return p
}

func parseCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	cert, err := k8spki.KeyPair{CertPEM: certPEM}.Certificate()
	if err != nil {
		t.Fatalf("Certificate(): %v", err)
	}
	return cert
}

func TestGenerateMakesKubeadmShapedCAs(t *testing.T) {
	t.Parallel()

	p := generate(t)
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}

	for _, tc := range []struct {
		name string
		pair k8spki.KeyPair
		cn   string
	}{
		{"cluster CA", p.CA, "kubernetes"},
		{"front-proxy CA", p.FrontProxyCA, "front-proxy-ca"},
		{"etcd CA", p.EtcdCA, "etcd-ca"},
	} {
		cert := parseCert(t, tc.pair.CertPEM)
		if cert.Subject.CommonName != tc.cn {
			t.Errorf("%s: CN = %q, want %q (kubeadm's own name)", tc.name, cert.Subject.CommonName, tc.cn)
		}
		if !cert.IsCA || !cert.BasicConstraintsValid {
			t.Errorf("%s: IsCA = %v, want a CA", tc.name, cert.IsCA)
		}
		if cert.KeyUsage&x509.KeyUsageCertSign == 0 {
			t.Errorf("%s: no CertSign key usage", tc.name)
		}
		if _, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok {
			t.Errorf("%s: key is %T, want ECDSA", tc.name, cert.PublicKey)
		}
		if got := cert.NotAfter.Sub(issued); got != k8spki.CAValidity {
			t.Errorf("%s: valid for %v, want %v", tc.name, got, k8spki.CAValidity)
		}
		// Backdated, so a node whose clock lags kumod's does not reject it.
		if !cert.NotBefore.Before(issued) {
			t.Errorf("%s: NotBefore = %v, want before the issue time", tc.name, cert.NotBefore)
		}
		// Self-signed.
		if err := cert.CheckSignatureFrom(cert); err != nil {
			t.Errorf("%s: not self-signed: %v", tc.name, err)
		}
	}

	// Three different CAs, not one key reused.
	if bytes.Equal(p.CA.CertPEM, p.EtcdCA.CertPEM) || bytes.Equal(p.CA.KeyPEM, p.FrontProxyCA.KeyPEM) {
		t.Error("two CAs share a certificate or a key")
	}
	// Two clusters never share a CA.
	other := generate(t)
	if bytes.Equal(p.CA.KeyPEM, other.CA.KeyPEM) {
		t.Error("two clusters were given the same CA key")
	}
}

func TestSystemCertificateIsKumodsAndNarrow(t *testing.T) {
	t.Parallel()

	p := generate(t)
	cert := parseCert(t, p.System.CertPEM)
	ca := parseCert(t, p.CA.CertPEM)

	if err := cert.CheckSignatureFrom(ca); err != nil {
		t.Fatalf("kumod's certificate is not signed by the cluster CA: %v", err)
	}
	if cert.Subject.CommonName != k8spki.SystemUser {
		t.Errorf("CN = %q, want %q", cert.Subject.CommonName, k8spki.SystemUser)
	}
	// kumo:system, never system:masters (K10).
	if got := cert.Subject.Organization; len(got) != 1 || got[0] != k8spki.GroupSystem {
		t.Errorf("O = %v, want [%s]", got, k8spki.GroupSystem)
	}
	if cert.IsCA {
		t.Error("kumod's client certificate is a CA")
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want client auth only", cert.ExtKeyUsage)
	}
}

// The kubeadm discovery hash is sha256 over the CA's SubjectPublicKeyInfo.
// Computed independently here, from the DER, so a change of format in CAHash
// fails a test rather than a join.
func TestCAHashIsKubeadmsFormat(t *testing.T) {
	t.Parallel()

	p := generate(t)
	got, err := k8spki.CAHash(p.CA.CertPEM)
	if err != nil {
		t.Fatalf("CAHash(): %v", err)
	}
	block, _ := pem.Decode(p.CA.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if want := "sha256:" + hex.EncodeToString(sum[:]); got != want {
		t.Errorf("CAHash() = %q, want %q", got, want)
	}
	if _, err := k8spki.CAHash([]byte("not a certificate")); !errors.Is(err, k8spki.ErrInvalidPKI) {
		t.Errorf("CAHash(garbage) error = %v, want ErrInvalidPKI", err)
	}
}

func TestMintKubeconfig(t *testing.T) {
	t.Parallel()

	p := generate(t)
	ca := parseCert(t, p.CA.CertPEM)
	const server = "https://api.orders.default:6443"

	for _, group := range []string{k8spki.GroupAdmins, k8spki.GroupViewers} {
		t.Run(group, func(t *testing.T) {
			t.Parallel()

			raw, err := p.MintKubeconfig(k8spki.KubeconfigRequest{
				ClusterName: "orders", Server: server, User: "ahmed", Group: group, Now: issued,
			}, nil)
			if err != nil {
				t.Fatalf("MintKubeconfig(): %v", err)
			}

			var doc struct {
				Clusters []struct {
					Cluster struct {
						Server string `yaml:"server"`
						CAData string `yaml:"certificate-authority-data"`
					} `yaml:"cluster"`
				} `yaml:"clusters"`
				Users []struct {
					User struct {
						Cert string `yaml:"client-certificate-data"`
						Key  string `yaml:"client-key-data"`
					} `yaml:"user"`
				} `yaml:"users"`
				CurrentContext string `yaml:"current-context"`
			}
			if err := yaml.Unmarshal(raw.Bytes(), &doc); err != nil {
				t.Fatalf("the kubeconfig is not YAML: %v", err)
			}
			if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != server {
				t.Fatalf("clusters = %+v, want the one server", doc.Clusters)
			}
			if doc.CurrentContext != "ahmed@orders" {
				t.Errorf("current-context = %q", doc.CurrentContext)
			}
			caPEM, _ := base64.StdEncoding.DecodeString(doc.Clusters[0].Cluster.CAData)
			if !bytes.Equal(caPEM, p.CA.CertPEM) {
				t.Error("the kubeconfig does not carry the cluster CA")
			}

			certPEM, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Cert)
			cert := parseCert(t, certPEM)
			if err := cert.CheckSignatureFrom(ca); err != nil {
				t.Errorf("the client certificate is not signed by the cluster CA: %v", err)
			}
			if cert.Subject.CommonName != "ahmed" || len(cert.Subject.Organization) != 1 ||
				cert.Subject.Organization[0] != group {
				t.Errorf("subject = %v, want CN=ahmed O=%s", cert.Subject, group)
			}
			if got := cert.NotAfter.Sub(issued); got != k8spki.KubeconfigValidity {
				t.Errorf("valid for %v, want %v", got, k8spki.KubeconfigValidity)
			}
			keyPEM, _ := base64.StdEncoding.DecodeString(doc.Users[0].User.Key)
			if _, err := (k8spki.KeyPair{CertPEM: certPEM, KeyPEM: keyPEM}).SignClient("x", "y", issued, time.Hour, nil); err != nil {
				// SignClient parses and matches the pair first; a key that does
				// not belong to the certificate fails there.
				t.Errorf("the key does not belong to the certificate: %v", err)
			}
		})
	}

	// Each download is a fresh certificate: nothing is stored to revoke.
	a, _ := p.MintKubeconfig(k8spki.KubeconfigRequest{ClusterName: "orders", Server: server, User: "ahmed", Group: k8spki.GroupAdmins, Now: issued}, nil)
	b, _ := p.MintKubeconfig(k8spki.KubeconfigRequest{ClusterName: "orders", Server: server, User: "ahmed", Group: k8spki.GroupAdmins, Now: issued}, nil)
	if bytes.Equal(a, b) {
		t.Error("two kubeconfigs share a key")
	}
}

func TestMintKubeconfigRefusals(t *testing.T) {
	t.Parallel()

	p := generate(t)
	base := k8spki.KubeconfigRequest{
		ClusterName: "orders", Server: "https://api.orders.default:6443",
		User: "ahmed", Group: k8spki.GroupAdmins, Now: issued,
	}
	for name, mutate := range map[string]func(*k8spki.KubeconfigRequest){
		// kumod's own group, and the unrevocable one, are never handed out.
		"kumo:system":    func(r *k8spki.KubeconfigRequest) { r.Group = k8spki.GroupSystem },
		"system:masters": func(r *k8spki.KubeconfigRequest) { r.Group = "system:masters" },
		"no group":       func(r *k8spki.KubeconfigRequest) { r.Group = "" },
		"plain http":     func(r *k8spki.KubeconfigRequest) { r.Server = "http://api.orders.default:6443" },
		"no server":      func(r *k8spki.KubeconfigRequest) { r.Server = "" },
		"no user":        func(r *k8spki.KubeconfigRequest) { r.User = "" },
		"no cluster":     func(r *k8spki.KubeconfigRequest) { r.ClusterName = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := base
			mutate(&req)
			if _, err := p.MintKubeconfig(req, nil); !errors.Is(err, k8spki.ErrInvalidPKI) {
				t.Errorf("MintKubeconfig() error = %v, want ErrInvalidPKI", err)
			}
		})
	}
}

func TestBootstrapToken(t *testing.T) {
	t.Parallel()

	tok, err := k8spki.NewBootstrapToken(nil)
	if err != nil {
		t.Fatalf("NewBootstrapToken(): %v", err)
	}
	if err := tok.Validate(); err != nil {
		t.Fatalf("Validate(): %v", err)
	}
	if !strings.HasPrefix(tok.Value(), tok.ID+".") || len(tok.Value()) != 23 {
		t.Errorf("Value() = %q, want <6>.<16>", tok.Value())
	}
	// Only the public id is ever printed.
	secret := string(tok.Secret.Bytes())
	for _, s := range []string{tok.String(), fmt.Sprintf("%v %+v %s", tok, tok, tok)} {
		if strings.Contains(s, secret) {
			t.Errorf("%q prints the token secret", s)
		}
	}

	other, _ := k8spki.NewBootstrapToken(nil)
	if other.Value() == tok.Value() {
		t.Error("two tokens are equal")
	}

	for _, bad := range []k8spki.BootstrapToken{
		{ID: "ABCDEF", Secret: k8spki.Secret("0123456789abcdef")},
		{ID: "abcdef", Secret: k8spki.Secret("short")},
		{ID: "abc", Secret: k8spki.Secret("0123456789abcdef")},
		{ID: "abcdef", Secret: k8spki.Secret("0123456789abcdeF")},
	} {
		if err := bad.Validate(); !errors.Is(err, k8spki.ErrInvalidPKI) {
			t.Errorf("Validate(%q) = %v, want ErrInvalidPKI", bad.ID, err)
		}
	}
}

// A key cannot leak by being formatted, logged or put in a JSON payload --
// the shapes a task payload or a log line would take.
func TestSecretsDoNotLeak(t *testing.T) {
	t.Parallel()

	p := generate(t)
	tok, _ := k8spki.NewBootstrapToken(nil)
	keys := [][]byte{p.CA.KeyPEM, p.EtcdCA.KeyPEM, p.FrontProxyCA.KeyPEM, p.ServiceAccountKey, p.System.KeyPEM}

	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.InfoContext(t.Context(), "pki", "pki", p, "ca_key", p.CA.KeyPEM, "token", tok)

	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", p),
		"%+v":  fmt.Sprintf("%+v", p),
		"%#v":  fmt.Sprintf("%#v", p),
		"%s":   fmt.Sprintf("%s", p.CA.KeyPEM), //nolint:gocritic // the verb is what is under test
		"%x":   fmt.Sprintf("%x", p.CA.KeyPEM),
		"slog": logged.String(),
	}
	for name, out := range outputs {
		if strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("%s prints a private key", name)
		}
		for _, k := range keys {
			// The base64 body of the key, which is what %v of a []byte would
			// otherwise expose as numbers or text.
			block, _ := pem.Decode(k)
			if strings.Contains(out, base64.StdEncoding.EncodeToString(block.Bytes)[:24]) {
				t.Errorf("%s prints key material", name)
			}
		}
		if strings.Contains(out, string(tok.Secret.Bytes())) {
			t.Errorf("%s prints the token secret", name)
		}
	}

	if _, err := json.Marshal(p); err == nil {
		t.Error("json.Marshal(pki) succeeded, want a refusal: a key has no place in a payload")
	}
}

func TestWriteAndLoadDir(t *testing.T) {
	t.Parallel()

	p := generate(t)
	dir := filepath.Join(t.TempDir(), "clusters", "eks-0123456789abcdef0")
	if err := k8spki.WriteDir(dir, p); err != nil {
		t.Fatalf("WriteDir(): %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("directory mode = %v, want 0700", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 10 {
		t.Errorf("the directory holds %d files, want 10", len(entries))
	}
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", e.Name(), fi.Mode().Perm())
		}
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}

	loaded, err := k8spki.LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir(): %v", err)
	}
	if !bytes.Equal(loaded.CA.CertPEM, p.CA.CertPEM) || !bytes.Equal(loaded.CA.KeyPEM, p.CA.KeyPEM) ||
		!bytes.Equal(loaded.ServiceAccountPub, p.ServiceAccountPub) || !bytes.Equal(loaded.System.KeyPEM, p.System.KeyPEM) {
		t.Error("LoadDir() returned different material than WriteDir() wrote")
	}

	// Generated once: a second write would lock kumod out of the cluster.
	if err := k8spki.WriteDir(dir, generate(t)); !errors.Is(err, os.ErrExist) {
		t.Errorf("WriteDir() over an existing CA error = %v, want ErrExist", err)
	}
	if again, _ := k8spki.LoadDir(dir); !bytes.Equal(again.CA.KeyPEM, p.CA.KeyPEM) {
		t.Error("the refused write changed the CA")
	}
}

func TestLoadDirRefusesDamage(t *testing.T) {
	t.Parallel()

	for name, damage := range map[string]func(dir string){
		"a missing file": func(dir string) { _ = os.Remove(filepath.Join(dir, "sa.pub")) },
		"a CA key from another cluster": func(dir string) {
			other, _ := k8spki.Generate("other", issued, nil)
			_ = os.WriteFile(filepath.Join(dir, "ca.key"), other.CA.KeyPEM, 0o600)
		},
		"kumod's certificate from another CA": func(dir string) {
			other, _ := k8spki.Generate("other", issued, nil)
			_ = os.WriteFile(filepath.Join(dir, "kumo.crt"), other.System.CertPEM, 0o600)
			_ = os.WriteFile(filepath.Join(dir, "kumo.key"), other.System.KeyPEM, 0o600)
		},
		"a truncated certificate": func(dir string) {
			_ = os.WriteFile(filepath.Join(dir, "etcd-ca.crt"), []byte("-----BEGIN CERTIFICATE-----\n"), 0o600)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := k8spki.WriteDir(dir, generate(t)); err != nil {
				t.Fatal(err)
			}
			damage(dir)
			if _, err := k8spki.LoadDir(dir); err == nil {
				t.Error("LoadDir() accepted a damaged directory")
			}
		})
	}
}
