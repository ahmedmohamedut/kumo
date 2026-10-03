package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// accessClaims are the access token claims of docs/design/01-api.md §Auth:
// sub, role, iat, exp and jti.
type accessClaims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

// refreshTokenBytes is the entropy of a refresh token (256 bits).
const refreshTokenBytes = 32

var (
	// ErrInvalidToken means a token is malformed, forged, expired or signed
	// with another key. Callers must answer with one uniform 401.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrWeakSecret means the signing key is shorter than HS256 allows.
	ErrWeakSecret = errors.New("auth: the signing key must be at least 32 bytes")
	// ErrBadAccessTTL means the configured access token lifetime is unusable.
	ErrBadAccessTTL = errors.New("auth: the access token lifetime must be positive")
	// ErrNoUserID means a token was requested without a user.
	ErrNoUserID = errors.New("auth: the user id must not be empty")
)

// Role is the authorization role carried by an access token.
type Role string

// The roles of the v1 authorization matrix.
const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// Principal is the identity behind a verified access token.
type Principal struct {
	UserID string
	Role   Role
}

// CanWrite reports whether the role may mutate resources. Viewers may only
// read, and answers 403 on writes (docs/design/01-api.md).
func (p Principal) CanWrite() bool { return p.Role == RoleAdmin }

// Signer issues and verifies access tokens. The signing method is HS256: the
// key never leaves the host, so there is nothing to gain from EdDSA yet
// (docs/design/NOTES.md, M1.1).
type Signer struct {
	secret []byte
	access time.Duration
	now    func() time.Time
}

// Option customizes a Signer.
type Option func(*Signer)

// WithClock overrides the clock the signer issues and validates against, so
// tests can move past a token's expiry without sleeping.
func WithClock(now func() time.Time) Option {
	return func(s *Signer) { s.now = now }
}

// NewSigner returns a Signer for an HMAC key and an access token lifetime.
func NewSigner(secret []byte, access time.Duration, opts ...Option) (*Signer, error) {
	if len(secret) < 32 {
		return nil, ErrWeakSecret
	}
	if access <= 0 {
		return nil, ErrBadAccessTTL
	}

	signer := &Signer{secret: secret, access: access, now: time.Now}
	for _, opt := range opts {
		opt(signer)
	}

	return signer, nil
}

// AccessToken is a freshly signed access token and its lifetime.
type AccessToken struct {
	Token     string
	ExpiresIn time.Duration
	ExpiresAt time.Time
}

// IssueAccessToken signs a token for a user. jti makes every token unique, so a
// leaked one can be told apart in the logs.
func (s *Signer) IssueAccessToken(userID string, role Role) (AccessToken, error) {
	if userID == "" {
		return AccessToken{}, ErrNoUserID
	}
	if role != RoleAdmin && role != RoleViewer {
		return AccessToken{}, fmt.Errorf("%w: unknown role %q", ErrInvalidToken, role)
	}

	now := s.now()
	expiresAt := now.Add(s.access)

	tokenID, err := randomHex(16)
	if err != nil {
		return AccessToken{}, err
	}

	claims := accessClaims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	signed, err := token.SignedString(s.secret)
	if err != nil {
		return AccessToken{}, fmt.Errorf("auth: sign access token: %w", err)
	}

	return AccessToken{Token: signed, ExpiresIn: s.access, ExpiresAt: expiresAt}, nil
}

// Verify checks an access token's signature and lifetime and returns its
// principal. Every failure is ErrInvalidToken: the caller must not be able to
// tell a forged token from an expired one.
func (s *Signer) Verify(token string) (Principal, error) {
	var claims accessClaims

	parsed, err := jwt.ParseWithClaims(token, &claims, s.keyFunc,
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithTimeFunc(s.now),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !parsed.Valid {
		return Principal{}, ErrInvalidToken
	}

	if claims.Role != RoleAdmin && claims.Role != RoleViewer {
		return Principal{}, ErrInvalidToken
	}
	if claims.Subject == "" {
		return Principal{}, ErrInvalidToken
	}

	return Principal{UserID: claims.Subject, Role: claims.Role}, nil
}

// keyFunc returns the signing key and rejects any other algorithm, which is
// what stops an attacker from presenting an "alg: none" or RS256 token.
func (s *Signer) keyFunc(token *jwt.Token) (any, error) {
	if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
		return nil, fmt.Errorf("%w: unexpected signing method %q", ErrInvalidToken, token.Method.Alg())
	}
	return s.secret, nil
}

// RefreshToken is a new opaque refresh token and the hash to persist. The plain
// token only ever travels in a cookie; the database stores the hash.
type RefreshToken struct {
	Token string
	Hash  string
}

// NewRefreshToken returns a 256-bit token and its SHA-256 hash.
func NewRefreshToken() (RefreshToken, error) {
	raw := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return RefreshToken{}, fmt.Errorf("auth: read refresh token: %w", err)
	}

	token := base64.RawURLEncoding.EncodeToString(raw)

	return RefreshToken{Token: token, Hash: HashRefreshToken(token)}, nil
}

// HashRefreshToken returns the storage form of a refresh token.
func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func randomHex(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return hex.EncodeToString(raw), nil
}
