// Package auth implements the authentication flows of docs/design/01-api.md:
// first-run setup, login, refresh-token rotation and logout. The HTTP layer in
// internal/api only translates requests into these calls.
package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Password policy of docs/design/01-api.md §Auth.
const (
	MinPasswordLength = 10
	MaxPasswordLength = 256
	MinUsernameLength = 3
	MaxUsernameLength = 64
)

// Options configures the service.
type Options struct {
	Logger     *slog.Logger
	Store      *store.Store
	Signer     *auth.Signer
	RefreshTTL time.Duration
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("auth: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("auth: a store is required")
	// ErrNoSigner means Options.Signer was nil.
	ErrNoSigner = errors.New("auth: a signer is required")
	// ErrBadRefreshTTL means the refresh token lifetime is unusable.
	ErrBadRefreshTTL = errors.New("auth: the refresh token lifetime must be positive")
)

// Service owns users and their refresh tokens.
type Service struct {
	logger     *slog.Logger
	store      *store.Store
	signer     *auth.Signer
	refreshTTL time.Duration
	limiter    *loginLimiter
	now        func() time.Time
}

// User is the safe projection of a user row.
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      auth.Role `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Session is what a login or a refresh returns: the access token travels in the
// Authorization header, the refresh token in a cookie.
type Session struct {
	User         User
	AccessToken  string
	AccessExpiry time.Duration
	RefreshToken string
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, ErrNoLogger
	}
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Signer == nil {
		return nil, ErrNoSigner
	}
	if opts.RefreshTTL <= 0 {
		return nil, ErrBadRefreshTTL
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Service{
		logger:     opts.Logger,
		store:      opts.Store,
		signer:     opts.Signer,
		refreshTTL: opts.RefreshTTL,
		limiter:    newLoginLimiter(loginAttempts, loginWindow, opts.Now),
		now:        opts.Now,
	}, nil
}

// CreateAdmin adds another administrator. It backs `kumod admin create`, which
// is how a second admin is made once the first-run setup is closed. The
// username is unique, case-insensitively.
func (s *Service) CreateAdmin(ctx context.Context, username, password string) (User, error) {
	if err := validateCredentials(username, password); err != nil {
		return User{}, err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	now := s.now().UTC()
	row, err := s.store.CreateUser(ctx, sqlc.CreateUserParams{
		ID:           id.ULID(),
		Username:     username,
		PasswordHash: hash,
		Role:         string(auth.RoleAdmin),
		CreatedAt:    formatTime(now),
		UpdatedAt:    formatTime(now),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, fmt.Errorf("%w: the username %q is taken", service.ErrAlreadyExists, username)
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}

	s.logger.InfoContext(ctx, "auth: administrator created", "user_id", row.ID, "username", row.Username)

	return userFromRow(row), nil
}

// ResetPassword replaces a user's password and revokes every refresh token they
// hold, so a reset signs out the sessions that may have been opened with the old
// password. It backs `kumod admin reset-password`, the v1 recovery path.
func (s *Service) ResetPassword(ctx context.Context, username, password string) error {
	if err := validateCredentials(username, password); err != nil {
		return err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	now := s.now().UTC()
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		user, err := tx.GetUserByUsername(ctx, sqlc.GetUserByUsernameParams{Username: username})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no user named %q", service.ErrNotFound, username)
			}
			return fmt.Errorf("get user by username: %w", err)
		}

		if _, err := tx.UpdateUserPassword(ctx, sqlc.UpdateUserPasswordParams{
			ID:           user.ID,
			PasswordHash: hash,
			UpdatedAt:    formatTime(now),
		}); err != nil {
			return fmt.Errorf("update user password: %w", err)
		}

		revoked, err := tx.RevokeRefreshTokensForUser(ctx, sqlc.RevokeRefreshTokensForUserParams{
			UserID:    user.ID,
			RevokedAt: nullableString(formatTime(now)),
		})
		if err != nil {
			return fmt.Errorf("revoke refresh tokens: %w", err)
		}

		s.logger.InfoContext(ctx, "auth: password reset",
			"user_id", user.ID, "username", user.Username, "revoked_tokens", revoked)

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// isUniqueViolation reports a UNIQUE constraint failure. SQLite error codes are
// matched on their text because modernc.org/sqlite does not export a typed
// error for them.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// Setup creates the first administrator. It is only allowed while the instance
// has no users; afterwards the endpoint answers 409.
func (s *Service) Setup(ctx context.Context, username, password string) (User, error) {
	users, err := s.store.CountUsers(ctx)
	if err != nil {
		return User{}, fmt.Errorf("count users: %w", err)
	}
	if users > 0 {
		return User{}, fmt.Errorf("%w: an administrator already exists", service.ErrConflict)
	}

	if err := validateCredentials(username, password); err != nil {
		return User{}, err
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash password: %w", err)
	}

	now := s.now().UTC()
	row, err := s.store.CreateUser(ctx, sqlc.CreateUserParams{
		ID:           id.ULID(),
		Username:     username,
		PasswordHash: hash,
		Role:         string(auth.RoleAdmin),
		CreatedAt:    formatTime(now),
		UpdatedAt:    formatTime(now),
	})
	if err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}

	s.logger.InfoContext(ctx, "auth: first administrator created", "user_id", row.ID, "username", row.Username)

	return userFromRow(row), nil
}

// errLoginFailed is the single answer to every credential problem. One message
// for an unknown username, a wrong password and an unreadable hash keeps the
// endpoint from enumerating users, and unlike the bare ErrUnauthorized sentinel
// it says something an operator reading the console can act on.
var errLoginFailed = fmt.Errorf("the username or password is wrong: %w", service.ErrUnauthorized)

// Login verifies a password and starts a refresh-token family. The failure is
// deliberately uniform: an unknown username, a wrong password and a corrupt
// hash all answer errLoginFailed, so the endpoint cannot be used to find out
// which usernames exist.
func (s *Service) Login(ctx context.Context, username, password, remoteAddr, userAgent string) (Session, error) {
	if s.limiter.blocked(remoteAddr, username) {
		s.logger.WarnContext(ctx, "auth: login rate limited",
			"remote_addr", remoteAddr, "username", username)
		return Session{}, fmt.Errorf("%w: too many attempts", service.ErrQuota)
	}

	row, err := s.store.GetUserByUsername(ctx, sqlc.GetUserByUsernameParams{Username: username})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.equalizeTiming()
			s.limiter.failed(remoteAddr, username)
			return Session{}, errLoginFailed
		}
		return Session{}, fmt.Errorf("get user by username: %w", err)
	}

	ok, err := auth.VerifyPassword(row.PasswordHash, password)
	if err != nil {
		// A hash this package cannot read is an operator problem, but answering
		// 500 would tell an attacker which usernames exist. Keep it uniform.
		s.logger.ErrorContext(ctx, "auth: stored password hash is unreadable", "user_id", row.ID, "error", err)
		return Session{}, errLoginFailed
	}
	if !ok {
		s.limiter.failed(remoteAddr, username)
		return Session{}, errLoginFailed
	}

	session, err := s.startSession(ctx, userFromRow(row), id.ULID(), userAgent)
	if err != nil {
		return Session{}, err
	}
	s.limiter.succeeded(remoteAddr, username)

	return session, nil
}

// Refresh rotates a refresh token. Presenting a token that was already revoked
// means it leaked, so the whole family is revoked and the caller has to log in
// again (docs/design/02-data-model.md, refresh_tokens.family_id).
func (s *Service) Refresh(ctx context.Context, token, userAgent string) (Session, error) {
	row, err := s.store.GetRefreshTokenByHash(ctx, sqlc.GetRefreshTokenByHashParams{
		TokenHash: auth.HashRefreshToken(token),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, service.ErrUnauthorized
		}
		return Session{}, fmt.Errorf("get refresh token: %w", err)
	}

	now := s.now().UTC()
	if row.RevokedAt.Valid {
		revoked, revokeErr := s.store.RevokeRefreshTokenFamily(ctx, sqlc.RevokeRefreshTokenFamilyParams{
			FamilyID:  row.FamilyID,
			RevokedAt: nullableString(formatTime(now)),
		})
		if revokeErr != nil {
			return Session{}, fmt.Errorf("revoke refresh token family: %w", revokeErr)
		}

		s.logger.WarnContext(ctx, "auth: refresh token reuse detected, family revoked",
			"user_id", row.UserID, "family_id", row.FamilyID, "revoked", revoked)

		return Session{}, service.ErrUnauthorized
	}

	if expired, err := time.Parse(time.RFC3339Nano, row.ExpiresAt); err != nil || !now.Before(expired) {
		if _, revokeErr := s.store.RevokeRefreshToken(ctx, sqlc.RevokeRefreshTokenParams{
			ID:        row.ID,
			RevokedAt: nullableString(formatTime(now)),
		}); revokeErr != nil {
			return Session{}, fmt.Errorf("revoke expired refresh token: %w", revokeErr)
		}
		return Session{}, service.ErrUnauthorized
	}

	user, err := s.store.GetUserByID(ctx, sqlc.GetUserByIDParams{ID: row.UserID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, service.ErrUnauthorized
		}
		return Session{}, fmt.Errorf("get user: %w", err)
	}

	return s.rotate(ctx, userFromRow(user), row, userAgent)
}

// Logout revokes the presented refresh token. It is idempotent: logging out
// twice, or with an unknown token, succeeds.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}

	now := s.now().UTC()
	row, err := s.store.GetRefreshTokenByHash(ctx, sqlc.GetRefreshTokenByHashParams{
		TokenHash: auth.HashRefreshToken(token),
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get refresh token: %w", err)
	}
	if row.RevokedAt.Valid {
		return nil
	}

	if _, err := s.store.RevokeRefreshToken(ctx, sqlc.RevokeRefreshTokenParams{
		ID:        row.ID,
		RevokedAt: nullableString(formatTime(now)),
	}); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}

	return nil
}

// Me returns the user behind a verified access token.
func (s *Service) Me(ctx context.Context, userID string) (User, error) {
	row, err := s.store.GetUserByID(ctx, sqlc.GetUserByIDParams{ID: userID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, service.ErrUnauthorized
		}
		return User{}, fmt.Errorf("get user: %w", err)
	}

	return userFromRow(row), nil
}

// NeedsSetup reports whether the first-run setup is still open.
func (s *Service) NeedsSetup(ctx context.Context) (bool, error) {
	users, err := s.store.CountUsers(ctx)
	if err != nil {
		return false, fmt.Errorf("count users: %w", err)
	}
	return users == 0, nil
}

// startSession signs an access token and opens a refresh-token family.
func (s *Service) startSession(
	ctx context.Context,
	user User,
	familyID string,
	userAgent string,
) (Session, error) {
	access, err := s.signer.IssueAccessToken(user.ID, user.Role)
	if err != nil {
		return Session{}, fmt.Errorf("issue access token: %w", err)
	}

	refresh, err := auth.NewRefreshToken()
	if err != nil {
		return Session{}, fmt.Errorf("create refresh token: %w", err)
	}

	now := s.now().UTC()
	if _, err := s.store.CreateRefreshToken(ctx, sqlc.CreateRefreshTokenParams{
		ID:        id.ULID(),
		UserID:    user.ID,
		TokenHash: refresh.Hash,
		FamilyID:  familyID,
		ExpiresAt: formatTime(now.Add(s.refreshTTL)),
		UserAgent: nullableString(userAgent),
		CreatedAt: formatTime(now),
	}); err != nil {
		return Session{}, fmt.Errorf("store refresh token: %w", err)
	}

	return Session{
		User:         user,
		AccessToken:  access.Token,
		AccessExpiry: access.ExpiresIn,
		RefreshToken: refresh.Token,
	}, nil
}

// rotate revokes the presented token and issues its successor in the same
// family, keeping the user signed in.
func (s *Service) rotate(
	ctx context.Context,
	user User,
	row sqlc.GetRefreshTokenByHashRow,
	userAgent string,
) (Session, error) {
	now := s.now().UTC()
	if _, err := s.store.RevokeRefreshToken(ctx, sqlc.RevokeRefreshTokenParams{
		ID:        row.ID,
		RevokedAt: nullableString(formatTime(now)),
	}); err != nil {
		return Session{}, fmt.Errorf("revoke refresh token: %w", err)
	}

	session, err := s.startSession(ctx, user, row.FamilyID, userAgent)
	if err != nil {
		return Session{}, err
	}

	return session, nil
}

// equalizeTiming spends the time a real verification would take when the
// username does not exist, so response time does not leak which users are real.
func (s *Service) equalizeTiming() {
	const rounds = 2
	for range rounds {
		_, _ = auth.HashPassword("kumo-timing-equalizer")
	}
}

func validateCredentials(username, password string) error {
	if fields := invalidCredentials(username, password); len(fields) > 0 {
		return &service.ValidationError{
			Message: "the credentials are not acceptable",
			Fields:  fields,
		}
	}
	return nil
}

func invalidCredentials(username, password string) []service.FieldError {
	var fields []service.FieldError

	switch {
	case len(username) < MinUsernameLength:
		fields = append(fields, service.FieldError{Field: "username", Reason: "too short"})
	case len(username) > MaxUsernameLength:
		fields = append(fields, service.FieldError{Field: "username", Reason: "too long"})
	case strings.TrimSpace(username) != username:
		fields = append(fields, service.FieldError{Field: "username", Reason: "must not start or end with whitespace"})
	}

	switch {
	case len(password) < MinPasswordLength:
		fields = append(fields, service.FieldError{Field: "password", Reason: "too short"})
	case len(password) > MaxPasswordLength:
		fields = append(fields, service.FieldError{Field: "password", Reason: "too long"})
	}

	return fields
}

func userFromRow(row sqlc.User) User {
	createdAt, _ := time.Parse(time.RFC3339Nano, row.CreatedAt) //nolint:errcheck // stored by this package

	return User{
		ID:        row.ID,
		Username:  row.Username,
		Role:      auth.Role(row.Role),
		CreatedAt: createdAt.UTC(),
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}
