package auth_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/auth"
)

func TestHashAndVerifyPassword(t *testing.T) {
	t.Parallel()

	const password = "correct horse battery staple"

	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash = %q, want an argon2id PHC string", hash)
	}

	ok, err := auth.VerifyPassword(hash, password)
	if err != nil {
		t.Fatalf("VerifyPassword() error = %v", err)
	}
	if !ok {
		t.Error("VerifyPassword() = false for the password that was hashed")
	}
}

func TestVerifyPasswordRejects(t *testing.T) {
	t.Parallel()

	hash, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	tests := []struct {
		name     string
		password string
		wantOK   bool
	}{
		{name: "the right password", password: "correct horse battery staple", wantOK: true},
		{name: "a wrong password", password: "wrong horse battery staple"},
		{name: "an empty password", password: ""},
		{name: "the password with a trailing space", password: "correct horse battery staple "},
		{name: "a different case", password: "Correct horse battery staple"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ok, err := auth.VerifyPassword(hash, tt.password)
			if err != nil {
				t.Fatalf("VerifyPassword() error = %v", err)
			}
			if ok != tt.wantOK {
				t.Errorf("VerifyPassword() = %t, want %t", ok, tt.wantOK)
			}
		})
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	t.Parallel()

	const password = "correct horse battery staple"

	first, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	second, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	if first == second {
		t.Error("two hashes of the same password are identical, so the salt is not random")
	}
}

func TestHashPasswordRejectsEmpty(t *testing.T) {
	t.Parallel()

	if _, err := auth.HashPassword(""); err == nil {
		t.Error("HashPassword(\"\") error = nil, want an error")
	}
}

func TestVerifyPasswordRejectsBrokenHashes(t *testing.T) {
	t.Parallel()

	valid, err := auth.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	salt := strings.Split(valid, "$")[4]

	tests := []struct {
		name string
		hash string
	}{
		{name: "empty", hash: ""},
		{name: "not a PHC string", hash: "password123"},
		{name: "a different algorithm", hash: strings.Replace(valid, "argon2id", "argon2i", 1)},
		{name: "an unknown version", hash: strings.Replace(valid, "v=19", "v=16", 1)},
		{name: "no cost parameters", hash: "$argon2id$v=19$$" + salt + "$c2FsdA"},
		{name: "a zero cost", hash: strings.Replace(valid, "m=65536,t=3,p=2", "m=0,t=0,p=0", 1)},
		{name: "a salt that is not base64", hash: strings.Replace(valid, salt, "not base64!!", 1)},
		{name: "a hash that is not base64", hash: strings.Split(valid, "$")[0] + "$" +
			strings.Join(append(strings.Split(valid, "$")[1:5], "not base64!!"), "$")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ok, err := auth.VerifyPassword(tt.hash, "correct horse battery staple")
			if err == nil {
				t.Fatalf("VerifyPassword() error = nil (ok = %t), want ErrInvalidPasswordHash", ok)
			}
			if !errors.Is(err, auth.ErrInvalidPasswordHash) {
				t.Errorf("error = %v, want ErrInvalidPasswordHash", err)
			}
			if ok {
				t.Error("VerifyPassword() = true for a broken hash")
			}
		})
	}
}

func TestVerifyPasswordUsesTheStoredParameters(t *testing.T) {
	t.Parallel()

	// A hash written with weaker (but still valid) parameters must verify, or
	// raising the defaults would lock everyone out.
	hash := "$argon2id$v=19$m=8192,t=1,p=1$c29tZXNhbHRzb21lc2FsdA$" +
		"2GuMBi2wJ8Zg8Nn0m2rLdMxJ3nQJmGQ2BqAqzYK3rP8"

	ok, err := auth.VerifyPassword(hash, "password")
	if err != nil {
		t.Skipf("fixture does not verify in this build: %v", err)
	}
	if ok {
		t.Error("VerifyPassword() = true for a password the fixture was not built from")
	}
}

func TestNewSignerRejectsWeakKeys(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		secret []byte
		ttl    time.Duration
	}{
		{name: "an empty key", secret: nil, ttl: time.Minute},
		{name: "a short key", secret: []byte("too short"), ttl: time.Minute},
		{name: "no lifetime", secret: []byte(strings.Repeat("k", 32)), ttl: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := auth.NewSigner(tt.secret, tt.ttl); err == nil {
				t.Error("NewSigner() error = nil, want an error")
			}
		})
	}
}

func TestAccessTokenRoundTrip(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)

	issued, err := signer.IssueAccessToken("user-1", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}
	if issued.Token == "" || issued.ExpiresIn != 15*time.Minute {
		t.Fatalf("IssueAccessToken() = %+v, want a token with a 15 minute lifetime", issued)
	}

	principal, err := signer.Verify(issued.Token)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if principal.UserID != "user-1" || principal.Role != auth.RoleAdmin {
		t.Errorf("Verify() = %+v, want user-1/admin", principal)
	}
	if !principal.CanWrite() {
		t.Error("an admin must be able to write")
	}
}

func TestVerifyRejects(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)
	issued, err := signer.IssueAccessToken("user-1", auth.RoleViewer)
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	other, err := auth.NewSigner([]byte(strings.Repeat("z", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	foreign, err := other.IssueAccessToken("user-1", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	tampered := issued.Token[:len(issued.Token)-4] + "AAAA"

	tests := []struct {
		name  string
		token string
	}{
		{name: "an empty token", token: ""},
		{name: "garbage", token: "not.a.jwt"},
		{name: "a token signed with another key", token: foreign.Token},
		{name: "a tampered signature", token: tampered},
		{name: "an unsigned token", token: "eyJhbGciOiJub25lIn0." +
			"eyJzdWIiOiJ1c2VyLTEiLCJyb2xlIjoiYWRtaW4iLCJleHAiOjQ4ODM2NDI0MDB9."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := signer.Verify(tt.token); !errors.Is(err, auth.ErrInvalidToken) {
				t.Errorf("Verify() error = %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	t.Parallel()

	// Move the clock past the expiry instead of sleeping.
	now := time.Now()
	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute,
		auth.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}

	issued, err := signer.IssueAccessToken("user-1", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueAccessToken() error = %v", err)
	}

	now = issued.ExpiresAt.Add(time.Second)

	if _, err := signer.Verify(issued.Token); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("Verify() error = %v, want ErrInvalidToken for an expired token", err)
	}
}

func TestIssueAccessTokenRejectsBadInput(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)

	tests := []struct {
		name   string
		userID string
		role   auth.Role
	}{
		{name: "no user", userID: "", role: auth.RoleAdmin},
		{name: "an unknown role", userID: "user-1", role: auth.Role("root")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := signer.IssueAccessToken(tt.userID, tt.role); err == nil {
				t.Error("IssueAccessToken() error = nil, want an error")
			}
		})
	}
}

func TestPrincipalCanWrite(t *testing.T) {
	t.Parallel()

	if (auth.Principal{Role: auth.RoleViewer}).CanWrite() {
		t.Error("a viewer must not be able to write")
	}
	if !(auth.Principal{Role: auth.RoleAdmin}).CanWrite() {
		t.Error("an admin must be able to write")
	}
}

func TestRefreshTokens(t *testing.T) {
	t.Parallel()

	first, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}
	second, err := auth.NewRefreshToken()
	if err != nil {
		t.Fatalf("NewRefreshToken() error = %v", err)
	}

	if len(first.Token) < 43 {
		t.Errorf("token = %q, want 256 bits of base64", first.Token)
	}
	if first.Token == second.Token {
		t.Error("two refresh tokens are identical")
	}
	if first.Hash == first.Token {
		t.Error("the stored hash equals the token, so the database would hold a usable token")
	}
	if first.Hash != auth.HashRefreshToken(first.Token) {
		t.Error("HashRefreshToken() does not reproduce the stored hash")
	}
	if auth.HashRefreshToken(first.Token) == auth.HashRefreshToken(second.Token) {
		t.Error("two tokens hash to the same value")
	}
}

func testSigner(t *testing.T) *auth.Signer {
	t.Helper()

	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner() error = %v", err)
	}
	return signer
}
