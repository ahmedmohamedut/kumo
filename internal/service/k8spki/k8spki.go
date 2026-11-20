// Package k8spki generates and holds a Kumo Kubernetes cluster's public key
// infrastructure: the cluster CA, the front-proxy and etcd CAs, the service
// account signing key, and kumod's own client certificate. It also mints the
// kubeconfigs users download and the bootstrap tokens nodes join with
// (docs/design/08-kubernetes.md section 5).
//
// kumod generates all of it before any VM boots, so the control plane's seed
// can hand kubeadm a complete PKI and a worker's seed can pin the CA by hash:
// every boot is self-sufficient, and every step of a create is idempotent.
//
// Everything is ECDSA P-256 from the standard library; nothing here needs a
// dependency. Private material is a Secret, which prints and logs as
// [redacted], so a key cannot reach a log line by being formatted.
package k8spki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"time"
)

// The groups Kumo's identities carry. Kumo's roles map to groups, never to
// system:masters, because a binding can be deleted and system:masters cannot be
// revoked short of replacing the CA (docs/design/08-kubernetes.md K10).
const (
	// GroupAdmins is bound to cluster-admin.
	GroupAdmins = "kumo:admins"
	// GroupViewers is bound to view.
	GroupViewers = "kumo:viewers"
	// GroupSystem is kumod's own identity, bound to a role with exactly the
	// verbs of section 6 and nothing else.
	GroupSystem = "kumo:system"
	// SystemUser is the common name of kumod's client certificate.
	SystemUser = "kumo"
)

// Validity periods.
const (
	// CAValidity is kubeadm's own default for the CAs it would generate.
	CAValidity = 10 * 365 * 24 * time.Hour
	// SystemValidity bounds kumod's own certificate. kumod holds the CA key,
	// so it can sign a fresh one whenever this nears its end.
	SystemValidity = 365 * 24 * time.Hour
	// KubeconfigValidity is how long a downloaded kubeconfig works (section 5).
	KubeconfigValidity = 30 * 24 * time.Hour
	// clockSkew backdates NotBefore, so a node whose clock runs a little
	// behind kumod's does not reject a certificate as not yet valid.
	clockSkew = 5 * time.Minute
)

var (
	// ErrInvalidPKI means stored or supplied PKI material does not parse or
	// does not belong together.
	ErrInvalidPKI = errors.New("k8spki: invalid PKI material")
	// ErrKeyMarshal is what marshaling a Secret to JSON answers.
	ErrKeyMarshal = errors.New("k8spki: refusing to marshal private key material")
)

// Secret is private key material, PEM-encoded. Its String, GoString and
// LogValue are [redacted], so formatting a struct that holds one, or logging
// it with slog, never prints the key. Bytes is the only way to the contents.
type Secret []byte

const redacted = "[redacted]"

// Bytes returns the PEM, for the few places that must write it: the seed and
// the PKI directory.
func (s Secret) Bytes() []byte { return []byte(s) }

func (s Secret) String() string   { return redacted }
func (s Secret) GoString() string { return redacted }

// LogValue keeps slog from printing the key.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// Format covers every fmt verb, %x and %s included.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

// MarshalJSON refuses: a key has no business in a JSON document, and a task
// payload or an API response that tried to carry one should fail loudly.
func (s Secret) MarshalJSON() ([]byte, error) {
	return nil, ErrKeyMarshal
}

// KeyPair is a certificate and its private key, both PEM.
type KeyPair struct {
	CertPEM []byte
	KeyPEM  Secret
}

// ClusterPKI is everything a cluster's control plane needs to start from
// kumod's keys rather than its own, and everything kumod keeps to operate it.
type ClusterPKI struct {
	// CA signs the API server, kubelet and every client certificate,
	// kubeconfigs included.
	CA KeyPair
	// FrontProxyCA signs the aggregation layer's client certificate.
	FrontProxyCA KeyPair
	// EtcdCA signs etcd's server, peer and client certificates.
	EtcdCA KeyPair
	// ServiceAccountKey signs service account tokens; ServiceAccountPub
	// verifies them.
	ServiceAccountKey Secret
	ServiceAccountPub []byte
	// System is kumod's own client certificate, in GroupSystem.
	System KeyPair
}

// Generate creates a cluster's PKI. now is the issue time; rnd is the source
// of randomness, crypto/rand.Reader when nil.
func Generate(clusterName string, now time.Time, rnd io.Reader) (*ClusterPKI, error) {
	if clusterName == "" {
		return nil, fmt.Errorf("%w: a cluster name is required", ErrInvalidPKI)
	}
	if rnd == nil {
		rnd = rand.Reader
	}

	var (
		p   ClusterPKI
		err error
	)
	// The common names are kubeadm's own, so a cluster looks like any other
	// kubeadm cluster to anybody inspecting its certificates.
	if p.CA, err = newCA("kubernetes", clusterName, now, rnd); err != nil {
		return nil, err
	}
	if p.FrontProxyCA, err = newCA("front-proxy-ca", clusterName, now, rnd); err != nil {
		return nil, err
	}
	if p.EtcdCA, err = newCA("etcd-ca", clusterName, now, rnd); err != nil {
		return nil, err
	}

	saKey, err := ecdsa.GenerateKey(elliptic.P256(), rnd)
	if err != nil {
		return nil, fmt.Errorf("k8spki: generate the service account key: %w", err)
	}
	if p.ServiceAccountKey, err = encodeKey(saKey); err != nil {
		return nil, err
	}
	pub, err := x509.MarshalPKIXPublicKey(&saKey.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("k8spki: encode the service account public key: %w", err)
	}
	p.ServiceAccountPub = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})

	if p.System, err = p.CA.SignClient(SystemUser, GroupSystem, now, SystemValidity, rnd); err != nil {
		return nil, err
	}
	return &p, nil
}

// newCA creates a self-signed CA.
func newCA(commonName, clusterName string, now time.Time, rnd io.Reader) (KeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rnd)
	if err != nil {
		return KeyPair{}, fmt.Errorf("k8spki: generate the %s key: %w", commonName, err)
	}
	serial, err := newSerial(rnd)
	if err != nil {
		return KeyPair{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: commonName,
			// The cluster's name in the subject, so two clusters' CAs are
			// told apart by eye in a support case.
			OrganizationalUnit: []string{"kumo cluster " + clusterName},
		},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(CAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rnd, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return KeyPair{}, fmt.Errorf("k8spki: sign the %s certificate: %w", commonName, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: encodeCert(der), KeyPEM: keyPEM}, nil
}

// SignClient signs a client certificate with this CA: common name user,
// organization group, valid from now for validity. Kubernetes reads the
// common name as the user and the organization as the group.
func (k KeyPair) SignClient(user, group string, now time.Time, validity time.Duration, rnd io.Reader) (KeyPair, error) {
	if user == "" || group == "" {
		return KeyPair{}, fmt.Errorf("%w: a client certificate needs a user and a group", ErrInvalidPKI)
	}
	if rnd == nil {
		rnd = rand.Reader
	}
	caCert, caKey, err := k.parse()
	if err != nil {
		return KeyPair{}, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rnd)
	if err != nil {
		return KeyPair{}, fmt.Errorf("k8spki: generate a client key: %w", err)
	}
	serial, err := newSerial(rnd)
	if err != nil {
		return KeyPair{}, err
	}
	notAfter := now.Add(validity)
	if notAfter.After(caCert.NotAfter) {
		// A leaf that outlived its CA would be rejected at that moment anyway;
		// stating it here keeps the certificate honest.
		notAfter = caCert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: user, Organization: []string{group}},
		NotBefore:    now.Add(-clockSkew),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rnd, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return KeyPair{}, fmt.Errorf("k8spki: sign the client certificate for %s: %w", user, err)
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{CertPEM: encodeCert(der), KeyPEM: keyPEM}, nil
}

// Certificate parses the certificate.
func (k KeyPair) Certificate() (*x509.Certificate, error) {
	block, _ := pem.Decode(k.CertPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: not a PEM certificate", ErrInvalidPKI)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPKI, err)
	}
	return cert, nil
}

// parse returns the certificate and its key, and checks that they belong
// together.
func (k KeyPair) parse() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cert, err := k.Certificate()
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(k.KeyPEM.Bytes())
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return nil, nil, fmt.Errorf("%w: not a PEM EC private key", ErrInvalidPKI)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidPKI, err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("%w: the key does not match the certificate", ErrInvalidPKI)
	}
	return cert, key, nil
}

// CAHash is the value `kubeadm join --discovery-token-ca-cert-hash` pins a CA
// by: sha256 over the certificate's SubjectPublicKeyInfo, as "sha256:<hex>".
// A worker that checks it cannot be talked into joining another cluster's
// control plane, whatever answers at the endpoint.
func CAHash(certPEM []byte) (string, error) {
	cert, err := KeyPair{CertPEM: certPEM}.Certificate()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func newSerial(rnd io.Reader) (*big.Int, error) {
	serial, err := rand.Int(rnd, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("k8spki: generate a serial number: %w", err)
	}
	return serial, nil
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// encodeKey writes an EC key in the SEC 1 form kubeadm's key loader reads.
func encodeKey(key *ecdsa.PrivateKey) (Secret, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("k8spki: encode a private key: %w", err)
	}
	return Secret(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// Validate checks that every pair parses and matches, and that the leaf
// certificates chain to the cluster CA. It is what LoadDir runs, so a damaged
// directory is found when it is read rather than when a node fails to join.
func (p *ClusterPKI) Validate() error {
	if p == nil {
		return fmt.Errorf("%w: no PKI", ErrInvalidPKI)
	}
	for name, pair := range map[string]KeyPair{
		"ca": p.CA, "front-proxy-ca": p.FrontProxyCA, "etcd-ca": p.EtcdCA, "kumo": p.System,
	} {
		if _, _, err := pair.parse(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	caCert, err := p.CA.Certificate()
	if err != nil {
		return err
	}
	sysCert, err := p.System.Certificate()
	if err != nil {
		return err
	}
	if err := sysCert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("%w: kumod's certificate is not signed by the cluster CA: %w", ErrInvalidPKI, err)
	}

	block, _ := pem.Decode(p.ServiceAccountKey.Bytes())
	if block == nil || block.Type != "EC PRIVATE KEY" {
		return fmt.Errorf("%w: the service account key is not a PEM EC private key", ErrInvalidPKI)
	}
	saKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("%w: service account key: %w", ErrInvalidPKI, err)
	}
	pubBlock, _ := pem.Decode(p.ServiceAccountPub)
	if pubBlock == nil {
		return fmt.Errorf("%w: the service account public key is not PEM", ErrInvalidPKI)
	}
	pub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		return fmt.Errorf("%w: service account public key: %w", ErrInvalidPKI, err)
	}
	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok || !ecPub.Equal(&saKey.PublicKey) {
		return fmt.Errorf("%w: the service account keys do not match", ErrInvalidPKI)
	}
	return nil
}
