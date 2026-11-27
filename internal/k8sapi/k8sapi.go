// Package k8sapi is kumod's client for a cluster's Kubernetes API: the six
// calls of docs/design/08-kubernetes.md section 6 -- health, nodes, bootstrap
// tokens, node labels, cordon, eviction and node deletion -- in plain HTTPS with kumod's
// client certificate and encoding/json, and nothing else (K11). client-go and
// its dependency tree stay out of the binary.
//
// kumod reaches a cluster from the host, which sits on every VPC bridge as its
// gateway, the way the databases readiness probe reaches 5432. The host does
// not use the VPC's resolver, so the client dials the control plane's address
// and checks the server's certificate against the endpoint's name, trusting the
// cluster CA and nothing else.
package k8sapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/service/k8spki"
)

// DefaultTimeout bounds one call, connection included. An API server that
// accepts a connection and never answers is a failed health check, not a
// worker stuck for good.
const DefaultTimeout = 15 * time.Second

// maxBody bounds what a response may make kumod read. A node or pod list for
// the clusters of this version is a few hundred KiB.
const maxBody = 8 << 20

var (
	// ErrUnavailable means the API server could not be reached or answered
	// with a server error: the cluster may be starting, or unhealthy.
	ErrUnavailable = errors.New("k8sapi: the API server is unavailable")
	// ErrNotReady means the API server answered /readyz with anything but ok.
	ErrNotReady = errors.New("k8sapi: the API server is not ready")
	// ErrForbidden means the API server refused kumod's identity (401/403):
	// its certificate is unknown, expired, or its role does not allow this.
	ErrForbidden = errors.New("k8sapi: the API server refused kumod's credential")
	// ErrNotFound means the object does not exist.
	ErrNotFound = errors.New("k8sapi: not found")
	// ErrEvictionBlocked means an eviction would break a PodDisruptionBudget
	// (429). A drain stops there rather than deleting past it (section 4).
	ErrEvictionBlocked = errors.New("k8sapi: the eviction is blocked by a disruption budget")
	// ErrInvalidConfig means the client could not be built.
	ErrInvalidConfig = errors.New("k8sapi: invalid configuration")
	// ErrBadResponse means the API server answered with something this client
	// cannot read.
	ErrBadResponse = errors.New("k8sapi: unreadable response")
)

// StatusError is an error status the API server returned, with its message.
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("k8sapi: %d %s: %s", e.Code, e.Reason, e.Message)
}

// Config is a client for one cluster.
type Config struct {
	// Address is the host:port dialed: the control plane's address and 6443.
	Address string
	// ServerName is the name the API server's certificate must carry, the
	// endpoint (api.<cluster>.<vpc>).
	ServerName string
	// CA is the cluster CA certificate, the only one trusted.
	CA []byte
	// ClientCert and ClientKey are kumod's kumo:system identity.
	ClientCert []byte
	ClientKey  k8spki.Secret
	// Timeout bounds one call; DefaultTimeout when zero.
	Timeout time.Duration
}

// Client calls one cluster's API server.
type Client struct {
	base string
	http *http.Client
}

// New builds a client.
func New(cfg Config) (*Client, error) {
	if _, _, err := net.SplitHostPort(cfg.Address); err != nil {
		return nil, fmt.Errorf("%w: address %q: %w", ErrInvalidConfig, cfg.Address, err)
	}
	if cfg.ServerName == "" {
		return nil, fmt.Errorf("%w: a server name is required", ErrInvalidConfig)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(cfg.CA) {
		return nil, fmt.Errorf("%w: the CA is not a PEM certificate", ErrInvalidConfig)
	}
	pair, err := tls.X509KeyPair(cfg.ClientCert, cfg.ClientKey.Bytes())
	if err != nil {
		return nil, fmt.Errorf("%w: client certificate: %w", ErrInvalidConfig, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:      roots,
			Certificates: []tls.Certificate{pair},
			ServerName:   cfg.ServerName,
			MinVersion:   tls.VersionTLS12,
		},
		// No proxy: the API server is on a VPC the host is the gateway of,
		// and an operator's HTTPS_PROXY must not see kumod's client
		// certificate handshake go elsewhere.
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: timeout}).DialContext,
		TLSHandshakeTimeout: timeout,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		base: "https://" + cfg.Address,
		http: &http.Client{Timeout: timeout, Transport: transport},
	}, nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Ready checks /readyz. It returns nil only for a 200 whose body is "ok".
func (c *Client) Ready(ctx context.Context) error {
	status, body, err := c.do(ctx, http.MethodGet, "/readyz", "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK || strings.TrimSpace(string(body)) != "ok" {
		return fmt.Errorf("%w: /readyz answered %d", ErrNotReady, status)
	}
	return nil
}

// Node is what kumod needs to know about a node.
type Node struct {
	Name          string
	Ready         bool
	Unschedulable bool
	InternalIP    string
	// Labels are the node's labels, nil when it has none.
	Labels map[string]string
}

// Nodes lists the cluster's nodes.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.getJSON(ctx, "/api/v1/nodes", &list); err != nil {
		return nil, err
	}
	nodes := make([]Node, 0, len(list.Items))
	for _, item := range list.Items {
		n := Node{Name: item.Metadata.Name, Unschedulable: item.Spec.Unschedulable, Labels: item.Metadata.Labels}
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" {
				n.Ready = cond.Status == "True"
			}
		}
		for _, a := range item.Status.Addresses {
			if a.Type == "InternalIP" {
				n.InternalIP = a.Address
			}
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

// bootstrapGroup is the group kubeadm's join flow expects a bootstrap token
// to carry.
const bootstrapGroup = "system:bootstrappers:kubeadm:default-node-token"

// CreateBootstrapToken adds a bootstrap token for a scale-up's new workers
// (section 4): a Secret in kube-system, in the shape kubeadm's own tokens
// take, expiring at expires.
func (c *Client) CreateBootstrapToken(ctx context.Context, token k8spki.BootstrapToken, expires time.Time) error {
	if err := token.Validate(); err != nil {
		return err
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "bootstrap.kubernetes.io/token",
		"metadata": map[string]any{
			"name":      "bootstrap-token-" + token.ID,
			"namespace": "kube-system",
			"labels":    map[string]string{"app.kubernetes.io/managed-by": "kumo"},
		},
		"stringData": map[string]string{
			"token-id":                       token.ID,
			"token-secret":                   string(token.Secret.Bytes()),
			"expiration":                     expires.UTC().Format(time.RFC3339),
			"usage-bootstrap-authentication": "true",
			"usage-bootstrap-signing":        "true",
			"auth-extra-groups":              bootstrapGroup,
		},
	}
	body, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("k8sapi: encode the bootstrap token: %w", err)
	}
	status, resp, err := c.do(ctx, http.MethodPost, "/api/v1/namespaces/kube-system/secrets",
		"application/json", body)
	if err != nil {
		return err
	}
	return expect(status, resp, http.StatusCreated)
}

// LabelNode sets labels on a node, leaving its others as they are: a merge
// patch of metadata.labels. An empty value is a label with no value, the shape
// of node-role.kubernetes.io/worker.
func (c *Client) LabelNode(ctx context.Context, node string, labels map[string]string) error {
	path, err := nodePath(node)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels}})
	if err != nil {
		return fmt.Errorf("k8sapi: encode the labels: %w", err)
	}
	status, resp, err := c.do(ctx, http.MethodPatch, path, "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	return expect(status, resp, http.StatusOK)
}

// Cordon marks a node unschedulable, the first step of a drain.
func (c *Client) Cordon(ctx context.Context, node string) error {
	path, err := nodePath(node)
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodPatch, path, "application/merge-patch+json",
		[]byte(`{"spec":{"unschedulable":true}}`))
	if err != nil {
		return err
	}
	return expect(status, resp, http.StatusOK)
}

// Pod is what a drain needs to know about a pod.
type Pod struct {
	Namespace string
	Name      string
	// DaemonSet pods are not evicted: their controller would put them back
	// on the node at once, and they go with the node.
	DaemonSet bool
	// Mirror pods are kubelet's static pods; the API cannot evict them.
	Mirror bool
}

// PodsOnNode lists the pods scheduled to a node.
func (c *Client) PodsOnNode(ctx context.Context, node string) ([]Pod, error) {
	if !namePattern.MatchString(node) {
		return nil, fmt.Errorf("%w: node name %q", ErrInvalidConfig, node)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace       string            `json:"namespace"`
				Name            string            `json:"name"`
				Annotations     map[string]string `json:"annotations"`
				OwnerReferences []struct {
					Kind string `json:"kind"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
		} `json:"items"`
	}
	query := url.Values{"fieldSelector": {"spec.nodeName=" + node}}
	if err := c.getJSON(ctx, "/api/v1/pods?"+query.Encode(), &list); err != nil {
		return nil, err
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, item := range list.Items {
		p := Pod{Namespace: item.Metadata.Namespace, Name: item.Metadata.Name}
		_, p.Mirror = item.Metadata.Annotations["kubernetes.io/config.mirror"]
		for _, ref := range item.Metadata.OwnerReferences {
			if ref.Kind == "DaemonSet" {
				p.DaemonSet = true
			}
		}
		pods = append(pods, p)
	}
	return pods, nil
}

// Evict asks the API server to evict a pod, which honors its
// PodDisruptionBudgets: ErrEvictionBlocked when one would be broken. A pod that
// is already gone is not an error.
func (c *Client) Evict(ctx context.Context, namespace, pod string) error {
	if !namePattern.MatchString(namespace) || !podNamePattern.MatchString(pod) {
		return fmt.Errorf("%w: pod %q/%q", ErrInvalidConfig, namespace, pod)
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": pod, "namespace": namespace},
	})
	if err != nil {
		return fmt.Errorf("k8sapi: encode the eviction: %w", err)
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(pod) + "/eviction"
	status, resp, err := c.do(ctx, http.MethodPost, path, "application/json", body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
		return nil
	case http.StatusNotFound:
		return nil
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s/%s: %w", ErrEvictionBlocked, namespace, pod, statusError(status, resp))
	default:
		return expect(status, resp, http.StatusCreated)
	}
}

// DeleteNode removes a node object, the last step before its VM goes. A node
// that is already gone is not an error.
func (c *Client) DeleteNode(ctx context.Context, node string) error {
	path, err := nodePath(node)
	if err != nil {
		return err
	}
	status, resp, err := c.do(ctx, http.MethodDelete, path, "", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	return expect(status, resp, http.StatusOK)
}

var (
	// namePattern is a DNS label or subdomain, the shape of a node or
	// namespace name; checked so nothing reaches a path it could change.
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	// podNamePattern is a DNS subdomain.
	podNamePattern = namePattern
)

func nodePath(node string) (string, error) {
	if !namePattern.MatchString(node) {
		return "", fmt.Errorf("%w: node name %q", ErrInvalidConfig, node)
	}
	return "/api/v1/nodes/" + url.PathEscape(node), nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	status, body, err := c.do(ctx, http.MethodGet, path, "", nil)
	if err != nil {
		return err
	}
	if err := expect(status, body, http.StatusOK); err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %s: %w", ErrBadResponse, path, err)
	}
	return nil
}

// do sends one request. A transport failure -- a refused connection, a
// timeout, a certificate that does not verify -- is ErrUnavailable wrapping the
// cause, so a caller can tell "unreachable" from an answer and still see why.
func (c *Client) do(ctx context.Context, method, path, contentType string, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("k8sapi: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: read %s %s: %w", ErrUnavailable, method, path, err)
	}
	return resp.StatusCode, data, nil
}

// expect maps a status other than want to the client's errors.
func expect(status int, body []byte, want int) error {
	if status == want {
		return nil
	}
	cause := statusError(status, body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrForbidden, cause)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrNotFound, cause)
	case status >= 500:
		return fmt.Errorf("%w: %w", ErrUnavailable, cause)
	default:
		return cause
	}
}

// statusError reads a Kubernetes Status object, or falls back to the body.
func statusError(code int, body []byte) *StatusError {
	var s struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &s) == nil && (s.Reason != "" || s.Message != "") {
		return &StatusError{Code: code, Reason: s.Reason, Message: s.Message}
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &StatusError{Code: code, Reason: http.StatusText(code), Message: msg}
}
