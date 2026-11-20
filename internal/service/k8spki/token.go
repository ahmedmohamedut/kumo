package k8spki

import (
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"regexp"
)

// tokenAlphabet is what a kubeadm bootstrap token is made of.
const tokenAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

var (
	tokenIDPattern     = regexp.MustCompile(`^[a-z0-9]{6}$`)
	tokenSecretPattern = regexp.MustCompile(`^[a-z0-9]{16}$`)
)

// BootstrapToken is a kubeadm bootstrap token, "<id>.<secret>". The id is
// public -- it names the token's Secret in kube-system -- and the secret is
// not, so only the id is ever printed.
type BootstrapToken struct {
	ID     string
	Secret Secret
}

// NewBootstrapToken generates a token from rnd, crypto/rand.Reader when nil.
// Each character is drawn uniformly from the 36 kubeadm allows.
func NewBootstrapToken(rnd io.Reader) (BootstrapToken, error) {
	if rnd == nil {
		rnd = rand.Reader
	}
	id, err := randomString(rnd, 6)
	if err != nil {
		return BootstrapToken{}, err
	}
	secret, err := randomString(rnd, 16)
	if err != nil {
		return BootstrapToken{}, err
	}
	return BootstrapToken{ID: id, Secret: Secret(secret)}, nil
}

// Validate reports whether the token has kubeadm's shape.
func (t BootstrapToken) Validate() error {
	if !tokenIDPattern.MatchString(t.ID) || !tokenSecretPattern.Match(t.Secret.Bytes()) {
		return fmt.Errorf("%w: not a bootstrap token", ErrInvalidPKI)
	}
	return nil
}

// Value is the whole token, for the seed that hands it to kubeadm.
func (t BootstrapToken) Value() string { return t.ID + "." + string(t.Secret.Bytes()) }

// String names the token by its public id only.
func (t BootstrapToken) String() string { return t.ID + "." + redacted }

// LogValue keeps slog to the id.
func (t BootstrapToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

func randomString(rnd io.Reader, n int) (string, error) {
	out := make([]byte, n)
	alphabet := big.NewInt(int64(len(tokenAlphabet)))
	for i := range out {
		v, err := rand.Int(rnd, alphabet)
		if err != nil {
			return "", fmt.Errorf("k8spki: generate a bootstrap token: %w", err)
		}
		out[i] = tokenAlphabet[v.Int64()]
	}
	return string(out), nil
}
