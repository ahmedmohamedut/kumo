package k8spki

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"time"

	"gopkg.in/yaml.v3"
)

// KubeconfigRequest is one kubeconfig to mint.
type KubeconfigRequest struct {
	// ClusterName names the cluster and the context in the file.
	ClusterName string
	// Server is the API endpoint, https://api.<cluster>.<vpc>:6443.
	Server string
	// User is the Kumo username, the certificate's common name, so the API
	// server's audit log names a person.
	User string
	// Group is GroupAdmins or GroupViewers, by the user's Kumo role.
	Group string
	// Now and Validity bound the certificate; Validity defaults to
	// KubeconfigValidity.
	Now      time.Time
	Validity time.Duration
}

// MintKubeconfig signs a fresh client certificate with the cluster CA and
// returns a kubeconfig carrying it. Nothing about it is stored: revoking every
// admin kubeconfig is replacing the kumo:admins binding, not finding files
// (docs/design/08-kubernetes.md section 5). The result holds a private key and
// is a Secret for that reason.
func (p *ClusterPKI) MintKubeconfig(req KubeconfigRequest, rnd io.Reader) (Secret, error) {
	switch req.Group {
	case GroupAdmins, GroupViewers:
	default:
		// GroupSystem is kumod's alone, and system:masters is never handed out.
		return nil, fmt.Errorf("%w: a kubeconfig is for %s or %s, not %q",
			ErrInvalidPKI, GroupAdmins, GroupViewers, req.Group)
	}
	if u, err := url.Parse(req.Server); err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("%w: server %q is not an https URL", ErrInvalidPKI, req.Server)
	}
	if req.ClusterName == "" || req.User == "" {
		return nil, fmt.Errorf("%w: a kubeconfig needs a cluster name and a user", ErrInvalidPKI)
	}
	validity := req.Validity
	if validity <= 0 {
		validity = KubeconfigValidity
	}

	client, err := p.CA.SignClient(req.User, req.Group, req.Now, validity, rnd)
	if err != nil {
		return nil, err
	}

	context := req.User + "@" + req.ClusterName
	doc := kubeconfig{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []namedCluster{{
			Name: req.ClusterName,
			Cluster: cluster{
				Server:                   req.Server,
				CertificateAuthorityData: base64.StdEncoding.EncodeToString(p.CA.CertPEM),
			},
		}},
		Users: []namedUser{{
			Name: req.User,
			User: user{
				ClientCertificateData: base64.StdEncoding.EncodeToString(client.CertPEM),
				ClientKeyData:         base64.StdEncoding.EncodeToString(client.KeyPEM.Bytes()),
			},
		}},
		Contexts: []namedContext{{
			Name:    context,
			Context: kubeContext{Cluster: req.ClusterName, User: req.User},
		}},
		CurrentContext: context,
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("k8spki: marshal the kubeconfig: %w", err)
	}
	return Secret(out), nil
}

// The subset of the kubeconfig format a client certificate login needs.
type kubeconfig struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	Clusters       []namedCluster `yaml:"clusters"`
	Users          []namedUser    `yaml:"users"`
	Contexts       []namedContext `yaml:"contexts"`
	CurrentContext string         `yaml:"current-context"`
}

type namedCluster struct {
	Name    string  `yaml:"name"`
	Cluster cluster `yaml:"cluster"`
}

type cluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

type namedUser struct {
	Name string `yaml:"name"`
	User user   `yaml:"user"`
}

type user struct {
	ClientCertificateData string `yaml:"client-certificate-data"`
	ClientKeyData         string `yaml:"client-key-data"`
}

type namedContext struct {
	Name    string      `yaml:"name"`
	Context kubeContext `yaml:"context"`
}

type kubeContext struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}
