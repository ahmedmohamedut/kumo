package k8sapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/k8sapi"
	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
)

const endpoint = "api.orders.default"

// fakeAPI answers like a kube-apiserver for the calls k8sapi makes. Like the
// real one, it requests a client certificate without requiring one at the TLS
// layer and checks it itself, so an unknown or expired certificate is a 401.
type fakeAPI struct {
	t      *testing.T
	pki    *k8spki.ClusterPKI
	server *httptest.Server

	mu       sync.Mutex
	requests []recorded
	ready    string
	// hang makes every request wait until the client gives up.
	hang bool
}

type recorded struct {
	method, path, query, contentType, user, group string
	body                                          []byte
}

func newFakeAPI(t *testing.T, pki *k8spki.ClusterPKI, serverCert tls.Certificate) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, pki: pki, ready: "ok"}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pki.CA.CertPEM)

	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, roots)
	}))
	f.server.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS12,
	}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request, roots *x509.CertPool) {
	if f.hang {
		<-r.Context().Done()
		return
	}
	if len(r.TLS.PeerCertificates) == 0 {
		status(w, http.StatusUnauthorized, "Unauthorized", "no client certificate")
		return
	}
	peer := r.TLS.PeerCertificates[0]
	if _, err := peer.Verify(x509.VerifyOptions{
		Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		status(w, http.StatusUnauthorized, "Unauthorized", err.Error())
		return
	}
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, recorded{
		method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
		contentType: r.Header.Get("Content-Type"),
		user:        peer.Subject.CommonName, group: strings.Join(peer.Subject.Organization, ","),
		body: body,
	})
	f.mu.Unlock()

	switch {
	case r.URL.Path == "/readyz":
		if f.ready != "ok" {
			w.WriteHeader(http.StatusInternalServerError)
		}
		_, _ = io.WriteString(w, f.ready)
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes":
		_, _ = io.WriteString(w, `{"kind":"NodeList","items":[
		  {"metadata":{"name":"orders-cp-1"},"spec":{},
		   "status":{"conditions":[{"type":"MemoryPressure","status":"False"},{"type":"Ready","status":"True"}],
		             "addresses":[{"type":"Hostname","address":"orders-cp-1"},{"type":"InternalIP","address":"192.168.100.10"}]}},
		  {"metadata":{"name":"orders-default-1"},"spec":{"unschedulable":true},
		   "status":{"conditions":[{"type":"Ready","status":"False"}],
		             "addresses":[{"type":"InternalIP","address":"192.168.100.11"}]}}]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/kube-system/secrets":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/nodes/orders-default-1":
		_, _ = io.WriteString(w, `{"kind":"Node"}`)
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/nodes/orders-default-1":
		_, _ = io.WriteString(w, `{"kind":"Status","status":"Success"}`)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/nodes/"):
		status(w, http.StatusNotFound, "NotFound", "nodes not found")
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/pods":
		_, _ = io.WriteString(w, `{"kind":"PodList","items":[
		  {"metadata":{"namespace":"default","name":"web-7c9f"}},
		  {"metadata":{"namespace":"kube-system","name":"cilium-x2k","ownerReferences":[{"kind":"DaemonSet"}]}},
		  {"metadata":{"namespace":"kube-system","name":"etcd-orders-cp-1","annotations":{"kubernetes.io/config.mirror":"abc"}}}]}`)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/eviction"):
		switch {
		case strings.Contains(r.URL.Path, "/pods/guarded/"):
			status(w, http.StatusTooManyRequests, "TooManyRequests",
				"Cannot evict pod as it would violate the pod's disruption budget.")
		case strings.Contains(r.URL.Path, "/pods/gone/"):
			status(w, http.StatusNotFound, "NotFound", `pods "gone" not found`)
		case strings.Contains(r.URL.Path, "/pods/locked/"):
			status(w, http.StatusForbidden, "Forbidden", "cannot create resource pods/eviction")
		default:
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"kind":"Status","status":"Success"}`)
		}
	default:
		status(w, http.StatusNotFound, "NotFound", r.Method+" "+r.URL.Path)
	}
}

func (f *fakeAPI) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("the API server received no request")
	}
	return f.requests[len(f.requests)-1]
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func status(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "status": "Failure",
		"reason": reason, "message": message, "code": code,
	})
}

// serverCert signs an API server certificate with ca, for names and the given
// validity.
func serverCert(t *testing.T, ca k8spki.KeyPair, names []string, notBefore, notAfter time.Time) tls.Certificate {
	t.Helper()
	caBlock, _ := pem.Decode(ca.CertPEM)
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyBlock, _ := pem.Decode(ca.KeyPEM.Bytes())
	caKey, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		DNSNames:     names,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type setup struct {
	pki    *k8spki.ClusterPKI
	api    *fakeAPI
	client *k8sapi.Client
}

func newSetup(t *testing.T) setup {
	t.Helper()
	now := time.Now()
	pki, err := k8spki.Generate("orders", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI(t, pki, serverCert(t, pki.CA, []string{endpoint}, now.Add(-time.Hour), now.Add(time.Hour)))
	return setup{pki: pki, api: api, client: clientFor(t, pki, api, pki.System, 0)}
}

func clientFor(t *testing.T, pki *k8spki.ClusterPKI, api *fakeAPI, id k8spki.KeyPair, timeout time.Duration) *k8sapi.Client {
	t.Helper()
	c, err := k8sapi.New(k8sapi.Config{
		Address:    api.server.Listener.Addr().String(),
		ServerName: endpoint,
		CA:         pki.CA.CertPEM,
		ClientCert: id.CertPEM,
		ClientKey:  id.KeyPEM,
		Timeout:    timeout,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestReady(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	if err := s.client.Ready(t.Context()); err != nil {
		t.Fatalf("Ready(): %v", err)
	}
	// kumod's own identity, as the API server saw it.
	if got := s.api.last(t); got.user != k8spki.SystemUser || got.group != k8spki.GroupSystem {
		t.Errorf("the API server saw %s / %s, want kumo / kumo:system", got.user, got.group)
	}

	s.api.ready = "[-]etcd failed: reason withheld"
	if err := s.client.Ready(t.Context()); !errors.Is(err, k8sapi.ErrNotReady) {
		t.Errorf("Ready() on an unhealthy server = %v, want ErrNotReady", err)
	}
}

func TestNodes(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	nodes, err := s.client.Nodes(t.Context())
	if err != nil {
		t.Fatalf("Nodes(): %v", err)
	}
	want := []k8sapi.Node{
		{Name: "orders-cp-1", Ready: true, InternalIP: "192.168.100.10"},
		{Name: "orders-default-1", Ready: false, Unschedulable: true, InternalIP: "192.168.100.11"},
	}
	if len(nodes) != len(want) {
		t.Fatalf("Nodes() = %+v", nodes)
	}
	for i := range want {
		if nodes[i] != want[i] {
			t.Errorf("node %d = %+v, want %+v", i, nodes[i], want[i])
		}
	}
}

func TestCreateBootstrapToken(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	tok, _ := k8spki.NewBootstrapToken(nil)
	expires := time.Date(2026, 11, 20, 13, 0, 0, 0, time.UTC)
	if err := s.client.CreateBootstrapToken(t.Context(), tok, expires); err != nil {
		t.Fatalf("CreateBootstrapToken(): %v", err)
	}

	req := s.api.last(t)
	var secret struct {
		Type     string `json:"type"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		StringData map[string]string `json:"stringData"`
	}
	if err := json.Unmarshal(req.body, &secret); err != nil {
		t.Fatalf("the secret is not JSON: %v", err)
	}
	// The shape kubeadm's own tokens take, so a join treats it as one.
	if secret.Type != "bootstrap.kubernetes.io/token" ||
		secret.Metadata.Name != "bootstrap-token-"+tok.ID || secret.Metadata.Namespace != "kube-system" {
		t.Errorf("secret = %s %s/%s", secret.Type, secret.Metadata.Namespace, secret.Metadata.Name)
	}
	for k, want := range map[string]string{
		"token-id": tok.ID, "token-secret": string(tok.Secret.Bytes()),
		"expiration":                     "2026-11-20T13:00:00Z",
		"usage-bootstrap-authentication": "true", "usage-bootstrap-signing": "true",
		"auth-extra-groups": "system:bootstrappers:kubeadm:default-node-token",
	} {
		if secret.StringData[k] != want {
			t.Errorf("%s = %q, want %q", k, secret.StringData[k], want)
		}
	}

	before := s.api.count()
	if err := s.client.CreateBootstrapToken(t.Context(), k8spki.BootstrapToken{ID: "x"}, expires); err == nil {
		t.Error("a malformed token was accepted")
	}
	if s.api.count() != before {
		t.Error("a malformed token reached the API server")
	}
}

func TestCordonAndDeleteNode(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	if err := s.client.Cordon(t.Context(), "orders-default-1"); err != nil {
		t.Fatalf("Cordon(): %v", err)
	}
	req := s.api.last(t)
	if req.method != http.MethodPatch || req.contentType != "application/merge-patch+json" ||
		string(req.body) != `{"spec":{"unschedulable":true}}` {
		t.Errorf("cordon = %s %s %q", req.method, req.contentType, req.body)
	}

	if err := s.client.DeleteNode(t.Context(), "orders-default-1"); err != nil {
		t.Errorf("DeleteNode(): %v", err)
	}
	// A node that is already gone is where a requeued scale-down lands.
	if err := s.client.DeleteNode(t.Context(), "orders-default-9"); err != nil {
		t.Errorf("DeleteNode() of a missing node = %v, want nil", err)
	}

	// A name that would change the path never leaves kumod.
	before := s.api.count()
	for _, bad := range []string{"../secrets", "orders/x", "", "Orders"} {
		if err := s.client.Cordon(t.Context(), bad); !errors.Is(err, k8sapi.ErrInvalidConfig) {
			t.Errorf("Cordon(%q) = %v, want ErrInvalidConfig", bad, err)
		}
	}
	if s.api.count() != before {
		t.Error("an invalid node name reached the API server")
	}
}

func TestPodsOnNode(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	pods, err := s.client.PodsOnNode(t.Context(), "orders-cp-1")
	if err != nil {
		t.Fatalf("PodsOnNode(): %v", err)
	}
	if q := s.api.last(t).query; q != "fieldSelector=spec.nodeName%3Dorders-cp-1" {
		t.Errorf("query = %q, want the node's field selector", q)
	}
	want := []k8sapi.Pod{
		{Namespace: "default", Name: "web-7c9f"},
		{Namespace: "kube-system", Name: "cilium-x2k", DaemonSet: true},
		{Namespace: "kube-system", Name: "etcd-orders-cp-1", Mirror: true},
	}
	if len(pods) != len(want) {
		t.Fatalf("PodsOnNode() = %+v", pods)
	}
	for i := range want {
		if pods[i] != want[i] {
			t.Errorf("pod %d = %+v, want %+v", i, pods[i], want[i])
		}
	}
}

func TestEvict(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	if err := s.client.Evict(t.Context(), "default", "web-7c9f"); err != nil {
		t.Fatalf("Evict(): %v", err)
	}
	req := s.api.last(t)
	var ev struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name, Namespace string
		} `json:"metadata"`
	}
	_ = json.Unmarshal(req.body, &ev)
	if req.path != "/api/v1/namespaces/default/pods/web-7c9f/eviction" ||
		ev.APIVersion != "policy/v1" || ev.Kind != "Eviction" || ev.Metadata.Name != "web-7c9f" {
		t.Errorf("eviction = %s %+v", req.path, ev)
	}

	// A disruption budget: the drain must stop, not delete past it.
	err := s.client.Evict(t.Context(), "default", "guarded")
	if !errors.Is(err, k8sapi.ErrEvictionBlocked) {
		t.Fatalf("Evict() of a guarded pod = %v, want ErrEvictionBlocked", err)
	}
	var st *k8sapi.StatusError
	if !errors.As(err, &st) || st.Code != http.StatusTooManyRequests || !strings.Contains(st.Message, "disruption budget") {
		t.Errorf("error = %v, want the API server's 429 message", err)
	}

	if err := s.client.Evict(t.Context(), "default", "gone"); err != nil {
		t.Errorf("Evict() of a pod already gone = %v, want nil", err)
	}
	if err := s.client.Evict(t.Context(), "default", "locked"); !errors.Is(err, k8sapi.ErrForbidden) {
		t.Errorf("Evict() refused by RBAC = %v, want ErrForbidden", err)
	}
	if err := s.client.Evict(t.Context(), "default", "../../nodes"); !errors.Is(err, k8sapi.ErrInvalidConfig) {
		t.Errorf("Evict() with a path in the name = %v, want ErrInvalidConfig", err)
	}
}

// The server's certificate must chain to the cluster CA, name the endpoint and
// be in date; a client that took any of these on trust would hand kumod's
// credential to whatever answers at the address.
func TestRefusesAServerItCannotTrust(t *testing.T) {
	t.Parallel()

	now := time.Now()
	pki, err := k8spki.Generate("orders", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := k8spki.Generate("other", now, nil)

	tests := map[string]struct {
		cert tls.Certificate
		want any
	}{
		"an expired certificate": {
			serverCert(t, pki.CA, []string{endpoint}, now.Add(-48*time.Hour), now.Add(-24*time.Hour)),
			&x509.CertificateInvalidError{},
		},
		"another cluster's CA": {
			serverCert(t, other.CA, []string{endpoint}, now.Add(-time.Hour), now.Add(time.Hour)),
			&x509.UnknownAuthorityError{},
		},
		"another name": {
			serverCert(t, pki.CA, []string{"api.other.default"}, now.Add(-time.Hour), now.Add(time.Hour)),
			&x509.HostnameError{},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newFakeAPI(t, pki, tc.cert)
			err := clientFor(t, pki, api, pki.System, 0).Ready(t.Context())
			if !errors.Is(err, k8sapi.ErrUnavailable) {
				t.Fatalf("Ready() = %v, want ErrUnavailable", err)
			}
			if !errors.As(err, &tc.want) {
				t.Errorf("Ready() = %v, want a %T", err, tc.want)
			}
			if api.count() != 0 {
				t.Error("a request reached a server the client should not have trusted")
			}
		})
	}
}

// An expired kumod certificate is refused by the API server, as a 401; the
// client reports it as a credential problem, not an outage.
func TestExpiredClientCertificate(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	expired, err := s.pki.CA.SignClient(k8spki.SystemUser, k8spki.GroupSystem,
		time.Now().Add(-72*time.Hour), time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := clientFor(t, s.pki, s.api, expired, 0)
	if _, err := c.Nodes(t.Context()); !errors.Is(err, k8sapi.ErrForbidden) {
		t.Errorf("Nodes() with an expired certificate = %v, want ErrForbidden", err)
	}
}

// An API server that accepts the connection and never answers is a failed
// call within the timeout, not a worker stuck for good.
func TestAHangingServerTimesOut(t *testing.T) {
	t.Parallel()

	s := newSetup(t)
	s.api.hang = true
	c := clientFor(t, s.pki, s.api, s.pki.System, 300*time.Millisecond)

	start := time.Now()
	err := c.Ready(t.Context())
	if !errors.Is(err, k8sapi.ErrUnavailable) {
		t.Errorf("Ready() = %v, want ErrUnavailable", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Ready() took %v against a 300ms timeout", elapsed)
	}
}

func TestNewRefusesABadConfig(t *testing.T) {
	t.Parallel()

	pki, _ := k8spki.Generate("orders", time.Now(), nil)
	good := k8sapi.Config{
		Address: "192.168.100.10:6443", ServerName: endpoint, CA: pki.CA.CertPEM,
		ClientCert: pki.System.CertPEM, ClientKey: pki.System.KeyPEM,
	}
	for name, mutate := range map[string]func(*k8sapi.Config){
		"no port":          func(c *k8sapi.Config) { c.Address = "192.168.100.10" },
		"no server name":   func(c *k8sapi.Config) { c.ServerName = "" },
		"no CA":            func(c *k8sapi.Config) { c.CA = nil },
		"a key of another": func(c *k8sapi.Config) { c.ClientKey = pki.CA.KeyPEM },
		"no client cert":   func(c *k8sapi.Config) { c.ClientCert = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := good
			mutate(&cfg)
			if _, err := k8sapi.New(cfg); !errors.Is(err, k8sapi.ErrInvalidConfig) {
				t.Errorf("New() = %v, want ErrInvalidConfig", err)
			}
		})
	}
}
