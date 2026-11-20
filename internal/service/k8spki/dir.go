package k8spki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The files of a cluster's PKI directory, $KUMO_DATA_DIR/clusters/<id>/
// (docs/design/08-kubernetes.md section 5). Named as kubeadm names them under
// /etc/kubernetes/pki where there is a kubeadm name, so the two are easy to
// compare by eye.
const (
	fileCA               = "ca.crt"
	fileCAKey            = "ca.key"
	fileFrontProxyCA     = "front-proxy-ca.crt"
	fileFrontProxyCAKey  = "front-proxy-ca.key"
	fileEtcdCA           = "etcd-ca.crt"
	fileEtcdCAKey        = "etcd-ca.key"
	fileServiceAccount   = "sa.key"
	fileServiceAccountPb = "sa.pub"
	fileSystem           = "kumo.crt"
	fileSystemKey        = "kumo.key"
)

// WriteDir writes the PKI into dir, created 0700. Every file is 0600, the
// certificates included: the directory is kumod's, nothing else reads it, and
// one mode for everything is one thing to check. A file is written to a
// temporary name and renamed, so a crash leaves either the old file or the new
// one, never half of either.
//
// WriteDir refuses a directory that already holds a CA: a cluster's PKI is
// generated once, and overwriting it would lock kumod out of a running
// cluster. A requeued create task reads the existing one with LoadDir instead.
func WriteDir(dir string, p *ClusterPKI) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("k8spki: create %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("k8spki: restrict %s: %w", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, fileCA)); err == nil {
		return fmt.Errorf("k8spki: %s already holds a cluster CA: %w", dir, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("k8spki: inspect %s: %w", dir, err)
	}

	files := []struct {
		name string
		data []byte
	}{
		// The CA certificate is written last: its presence is what marks the
		// directory as complete (above, and in LoadDir's error).
		{fileCAKey, p.CA.KeyPEM.Bytes()},
		{fileFrontProxyCA, p.FrontProxyCA.CertPEM},
		{fileFrontProxyCAKey, p.FrontProxyCA.KeyPEM.Bytes()},
		{fileEtcdCA, p.EtcdCA.CertPEM},
		{fileEtcdCAKey, p.EtcdCA.KeyPEM.Bytes()},
		{fileServiceAccount, p.ServiceAccountKey.Bytes()},
		{fileServiceAccountPb, p.ServiceAccountPub},
		{fileSystem, p.System.CertPEM},
		{fileSystemKey, p.System.KeyPEM.Bytes()},
		{fileCA, p.CA.CertPEM},
	}
	for _, f := range files {
		if err := writeFileAtomic(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}
	return nil
}

// LoadDir reads and validates the PKI WriteDir wrote.
func LoadDir(dir string) (*ClusterPKI, error) {
	read := func(name string) ([]byte, error) {
		b, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the path is kumod's own data directory
		if err != nil {
			return nil, fmt.Errorf("k8spki: read %s: %w", name, err)
		}
		return b, nil
	}

	var (
		p   ClusterPKI
		err error
		b   []byte
	)
	load := []struct {
		name string
		set  func([]byte)
	}{
		{fileCA, func(v []byte) { p.CA.CertPEM = v }},
		{fileCAKey, func(v []byte) { p.CA.KeyPEM = v }},
		{fileFrontProxyCA, func(v []byte) { p.FrontProxyCA.CertPEM = v }},
		{fileFrontProxyCAKey, func(v []byte) { p.FrontProxyCA.KeyPEM = v }},
		{fileEtcdCA, func(v []byte) { p.EtcdCA.CertPEM = v }},
		{fileEtcdCAKey, func(v []byte) { p.EtcdCA.KeyPEM = v }},
		{fileServiceAccount, func(v []byte) { p.ServiceAccountKey = v }},
		{fileServiceAccountPb, func(v []byte) { p.ServiceAccountPub = v }},
		{fileSystem, func(v []byte) { p.System.CertPEM = v }},
		{fileSystemKey, func(v []byte) { p.System.KeyPEM = v }},
	}
	for _, f := range load {
		if b, err = read(f.name); err != nil {
			return nil, err
		}
		f.set(b)
	}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("k8spki: %s: %w", dir, err)
	}
	return &p, nil
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("k8spki: write %s: %w", path, err)
	}
	name := tmp.Name()
	defer os.Remove(name) //nolint:errcheck // gone after the rename; this covers the error paths

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("k8spki: restrict %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("k8spki: write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("k8spki: flush %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("k8spki: close %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("k8spki: place %s: %w", path, err)
	}
	return nil
}
