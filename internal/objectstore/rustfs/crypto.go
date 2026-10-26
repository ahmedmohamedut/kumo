package rustfs

import (
	"bytes"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/secure-io/sio-go"
)

// The RustFS admin API is MinIO's, and like MinIO it encrypts the bodies that
// carry credentials (a new user's secret, the user list) with the admin's
// secret key. The format is madmin's: a 32-byte salt, one algorithm byte, the
// stream nonce, then a sio (DARE) stream.
//
// Kumo writes PBKDF2-SHA256 with AES-256-GCM, which is what RustFS itself
// answers with, and reads that and the two argon2id variants are refused: no
// server Kumo talks to sends them. madmin-go is not used because it is
// AGPL-3.0; this file is the part of it Kumo needs.
const (
	saltSize = 32
	// algPBKDF2AESGCM is madmin's id for PBKDF2-SHA256 + AES-256-GCM.
	algPBKDF2AESGCM = 0x02
	// pbkdf2Iterations is madmin's pbkdf2Cost.
	pbkdf2Iterations = 8192
	keySize          = 32
)

// ErrAdminCipher means an admin body is not in the format above, or does not
// decrypt with the admin secret key.
var ErrAdminCipher = errors.New("rustfs: an admin body could not be decrypted")

// encryptAdmin seals data for the admin API with the admin secret key.
func encryptAdmin(secretKey string, data []byte) ([]byte, error) {
	salt := make([]byte, saltSize)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("rustfs: salt: %w", err)
	}
	stream, err := adminStream(secretKey, salt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, stream.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("rustfs: nonce: %w", err)
	}

	var out bytes.Buffer
	out.Write(salt)
	out.WriteByte(algPBKDF2AESGCM)
	out.Write(nonce)
	w := stream.EncryptWriter(&out, nonce, nil)
	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("rustfs: encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("rustfs: encrypt: %w", err)
	}
	return out.Bytes(), nil
}

// decryptAdmin opens a body the admin API sealed with the admin secret key.
func decryptAdmin(secretKey string, data []byte) ([]byte, error) {
	if len(data) < saltSize+1 {
		return nil, fmt.Errorf("%w: %d bytes is too short", ErrAdminCipher, len(data))
	}
	salt, alg, rest := data[:saltSize], data[saltSize], data[saltSize+1:]
	if alg != algPBKDF2AESGCM {
		return nil, fmt.Errorf("%w: algorithm %#x is not supported", ErrAdminCipher, alg)
	}
	stream, err := adminStream(secretKey, salt)
	if err != nil {
		return nil, err
	}
	if len(rest) < stream.NonceSize() {
		return nil, fmt.Errorf("%w: no nonce", ErrAdminCipher)
	}
	nonce, sealed := rest[:stream.NonceSize()], rest[stream.NonceSize():]
	plain, err := io.ReadAll(stream.DecryptReader(bytes.NewReader(sealed), nonce, nil))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAdminCipher, err)
	}
	return plain, nil
}

func adminStream(secretKey string, salt []byte) (*sio.Stream, error) {
	key, err := pbkdf2.Key(sha256.New, secretKey, salt, pbkdf2Iterations, keySize)
	if err != nil {
		return nil, fmt.Errorf("rustfs: derive the admin key: %w", err)
	}
	stream, err := sio.AES_256_GCM.Stream(key)
	if err != nil {
		return nil, fmt.Errorf("rustfs: admin cipher: %w", err)
	}
	return stream, nil
}
