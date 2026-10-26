package rustfs

import (
	"bytes"
	"errors"
	"testing"
)

func TestAdminCipherRoundTrip(t *testing.T) {
	for _, size := range []int{0, 1, 100, 16*1024 - 1, 16 * 1024, 100_000} {
		data := bytes.Repeat([]byte{'k'}, size)
		sealed, err := encryptAdmin("admin-secret", data)
		if err != nil {
			t.Fatalf("encrypt %d bytes: %v", size, err)
		}
		if sealed[saltSize] != algPBKDF2AESGCM {
			t.Errorf("algorithm byte %#x", sealed[saltSize])
		}
		// A short plaintext turns up in random salt and ciphertext by chance (a
		// single byte about one time in five), so only a run long enough to
		// mean something is looked for.
		if size >= 16 && bytes.Contains(sealed, data) {
			t.Errorf("%d bytes: the plaintext is visible in the sealed body", size)
		}
		plain, err := decryptAdmin("admin-secret", sealed)
		if err != nil || !bytes.Equal(plain, data) {
			t.Errorf("round trip of %d bytes: %d bytes back, %v", size, len(plain), err)
		}
	}
}

func TestAdminCipherRefusals(t *testing.T) {
	sealed, err := encryptAdmin("admin-secret", []byte(`{"secretKey":"s"}`))
	if err != nil {
		t.Fatal(err)
	}

	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1
	argon := bytes.Clone(sealed)
	argon[saltSize] = 0x00

	for name, tt := range map[string]struct {
		password string
		data     []byte
	}{
		"wrong password": {"other", sealed},
		"tampered":       {"admin-secret", tampered},
		"short":          {"admin-secret", sealed[:10]},
		"argon2id":       {"admin-secret", argon},
	} {
		if _, err := decryptAdmin(tt.password, tt.data); !errors.Is(err, ErrAdminCipher) {
			t.Errorf("%s: got %v, want ErrAdminCipher", name, err)
		}
	}
}
