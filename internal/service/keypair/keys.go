// Package keypair owns SSH key pairs: generation, import validation and
// storage in the metadata database. A generated private key lives only in
// memory and in the one create response; it is never persisted and never
// logged (docs/design/01-api.md §Key pairs, AGENTS.md security rules).
package keypair

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/ahmedmohamedut/kumo/internal/service"
)

// rsaGenerateBits is the size of a generated RSA key pair: the "rsa-4096" of
// the API. rsaImportMinBits is the floor for imported keys, matching the
// AWS rule 01-api.md documents.
const (
	rsaGenerateBits  = 4096
	rsaImportMinBits = 2048
)

// material is the full key material of a freshly generated pair. privatePEM
// exists exactly once: it goes into the create response and nowhere else.
type material struct {
	publicLine  string
	privatePEM  string
	fingerprint string
}

// generate mints a new key pair of the given type. The caller must have
// validated the type already; anything else is a programming error.
func generate(keyType Type, comment string) (material, error) {
	var (
		priv any
		err  error
	)

	switch keyType {
	case TypeED25519:
		var key ed25519.PrivateKey
		_, key, err = ed25519.GenerateKey(rand.Reader)
		priv = key
	case TypeRSA:
		priv, err = rsa.GenerateKey(rand.Reader, rsaGenerateBits)
	default:
		return material{}, fmt.Errorf("%w: unsupported key type %q", service.ErrInvalid, keyType)
	}
	if err != nil {
		return material{}, fmt.Errorf("generate %s key: %w", keyType, err)
	}

	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return material{}, fmt.Errorf("wrap %s key: %w", keyType, err)
	}

	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return material{}, fmt.Errorf("marshal private key: %w", err)
	}

	return material{
		publicLine:  authorizedLine(signer.PublicKey(), comment),
		privatePEM:  string(pem.EncodeToMemory(block)),
		fingerprint: fingerprintOf(signer.PublicKey()),
	}, nil
}

// privateKeyMarker detects private key material pasted into the public key
// field, OpenSSH or PKCS#8 PEM armour alike.
const privateKeyMarker = "PRIVATE KEY-----"

// parsePublicKey validates one OpenSSH authorized_keys line and returns its
// canonical form. It refuses private key material, unsupported algorithms,
// lines with authorized_keys options, more than one key, and RSA keys below
// the 2048 bit floor of 01-api.md.
//
// Validation failures come back as a *service.ValidationError, so the import
// endpoint answers 400 instead of 500.
func parsePublicKey(line string) (publicLine string, keyType Type, fingerprint string, err error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", "", "", invalidPublicKey("must not be empty")
	}
	if strings.Contains(line, privateKeyMarker) {
		return "", "", "", invalidPublicKey("private key material must not be imported")
	}

	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(trimmed))
	if err != nil {
		return "", "", "", invalidPublicKey("is not an OpenSSH public key")
	}
	if len(options) > 0 {
		return "", "", "", invalidPublicKey("authorized_keys options are not supported, pass only the key")
	}
	if strings.TrimSpace(string(rest)) != "" {
		return "", "", "", invalidPublicKey("import exactly one key")
	}

	switch key.Type() {
	case ssh.KeyAlgoED25519:
		keyType = TypeED25519
	case ssh.KeyAlgoRSA:
		cryptoKey, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return "", "", "", invalidPublicKey("is not an OpenSSH public key")
		}
		rsaKey, ok := cryptoKey.CryptoPublicKey().(*rsa.PublicKey)
		if !ok {
			return "", "", "", invalidPublicKey("is not an OpenSSH public key")
		}
		if bits := rsaKey.N.BitLen(); bits < rsaImportMinBits {
			return "", "", "", invalidPublicKey(
				fmt.Sprintf("RSA keys must be at least %d bits, got %d", rsaImportMinBits, bits))
		}
		keyType = TypeRSA
	default:
		return "", "", "", invalidPublicKey(
			fmt.Sprintf("key type %q is not supported, use ed25519 or rsa", key.Type()))
	}

	return authorizedLine(key, comment), keyType, fingerprintOf(key), nil
}

// invalidPublicKey builds the validation failure import reports on the
// public_key field.
func invalidPublicKey(reason string) *service.ValidationError {
	return &service.ValidationError{
		Message: "the public key is not acceptable",
		Fields:  []service.FieldError{{Field: "public_key", Reason: reason}},
	}
}

// authorizedLine renders the canonical authorized_keys form: type, base64
// blob and the comment, so a stored key always compares equal to itself
// regardless of the whitespace or padding the caller pasted.
func authorizedLine(key ssh.PublicKey, comment string) string {
	line := key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
	if comment != "" {
		line += " " + comment
	}
	return line
}

// fingerprintOf is the OpenSSH fingerprint ssh-keygen -lf prints: the raw
// SHA-256 of the wire format, unpadded standard base64 behind a SHA256: label.
func fingerprintOf(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
