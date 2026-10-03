package auth_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	kumoauth "github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

const (
	testPassword   = "correct horse battery staple"
	testRefreshTTL = 7 * 24 * time.Hour
)

func TestSetupCreatesTheFirstAdministrator(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)

	user, err := svc.Setup(t.Context(), "ada", testPassword)
	if err != nil {
		t.Fatalf("Setup() error = %v", err)
	}
	if user.Username != "ada" || user.Role != kumoauth.RoleAdmin {
		t.Errorf("Setup() = %+v, want the admin ada", user)
	}
	if user.CreatedAt.IsZero() {
		t.Error("Setup() returned a user without a creation time")
	}

	needs, err := svc.NeedsSetup(t.Context())
	if err != nil {
		t.Fatalf("NeedsSetup() error = %v", err)
	}
	if needs {
		t.Error("NeedsSetup() = true after the administrator was created")
	}
}

func TestSetupIsOnlyOpenOnce(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	// A second setup must not create a second administrator, even from another
	// caller.
	if _, err := svc.Setup(t.Context(), "eve", testPassword); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Setup() error = %v, want ErrConflict", err)
	}

	if _, err := svc.Login(t.Context(), "eve", testPassword, "", ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Login() error = %v, want ErrUnauthorized for a user setup never created", err)
	}
}

func TestSetupRejectsWeakCredentials(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		username string
		password string
		field    string
	}{
		{name: "a short username", username: "ab", password: testPassword, field: "username"},
		{name: "a long username", username: strings.Repeat("a", 65), password: testPassword, field: "username"},
		{name: "a padded username", username: " ada", password: testPassword, field: "username"},
		{name: "a short password", username: "ada", password: "123456789", field: "password"},
		{name: "a long password", username: "ada", password: strings.Repeat("a", 257), field: "password"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			svc, _, _ := newService(t)

			_, err := svc.Setup(t.Context(), tt.username, tt.password)
			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Setup() error = %v, want a ValidationError", err)
			}
			if len(invalid.Fields) != 1 || invalid.Fields[0].Field != tt.field {
				t.Errorf("fields = %+v, want one error on %q", invalid.Fields, tt.field)
			}

			needs, err := svc.NeedsSetup(t.Context())
			if err != nil {
				t.Fatalf("NeedsSetup() error = %v", err)
			}
			if !needs {
				t.Error("a rejected setup created a user anyway")
			}
		})
	}
}

func TestSetupStoresOnlyTheHash(t *testing.T) {
	t.Parallel()

	svc, db, _ := newService(t)
	mustSetup(t, svc, "ada")

	var hash string
	err := db.DB().QueryRowContext(t.Context(),
		`SELECT password_hash FROM users WHERE username = 'ada'`).Scan(&hash)
	if err != nil {
		t.Fatalf("read users: %v", err)
	}

	if strings.Contains(hash, testPassword) {
		t.Error("the password hash contains the plaintext password")
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("password_hash = %q, want an argon2id hash", hash)
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session, err := svc.Login(t.Context(), "ada", testPassword, "203.0.113.4", "curl/8")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if session.AccessToken == "" || session.RefreshToken == "" {
		t.Errorf("Login() = %+v, want both tokens", session)
	}
	if session.AccessExpiry != 15*time.Minute {
		t.Errorf("AccessExpiry = %v, want 15m", session.AccessExpiry)
	}
	if session.User.Username != "ada" || session.User.Role != kumoauth.RoleAdmin {
		t.Errorf("User = %+v, want the admin ada", session.User)
	}
}

func TestLoginIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	if _, err := svc.Login(t.Context(), "Ada", testPassword, "", ""); err != nil {
		t.Errorf("Login() error = %v, want the username to match case-insensitively", err)
	}
}

func TestLoginRejectsAWrongPassword(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session, err := svc.Login(t.Context(), "ada", "wrong password", "", "")
	if !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("Login() error = %v, want ErrUnauthorized", err)
	}
	if session.AccessToken != "" || session.RefreshToken != "" {
		t.Errorf("Login() = %+v, want no tokens", session)
	}
}

func TestLoginAnswersTheSameWayForAnUnknownUser(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	_, err := svc.Login(t.Context(), "eve", "whatever password", "", "")
	if !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("Login() error = %v, want ErrUnauthorized", err)
	}
	if strings.Contains(err.Error(), "eve") {
		t.Errorf("error = %q, want no hint that the username exists or not", err)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	t.Parallel()

	svc, _, clock := newService(t)
	mustSetup(t, svc, "ada")

	const addr = "203.0.113.4"
	for attempt := range 5 {
		if _, err := svc.Login(t.Context(), "ada", "wrong password", addr, ""); !errors.Is(err, service.ErrUnauthorized) {
			t.Fatalf("attempt %d: error = %v, want ErrUnauthorized", attempt+1, err)
		}
	}

	// The right password does not buy a way past the limiter.
	if _, err := svc.Login(t.Context(), "ada", testPassword, addr, ""); !errors.Is(err, service.ErrQuota) {
		t.Errorf("Login() error = %v, want ErrQuota after 5 failures", err)
	}

	// Another address and another user have their own budget.
	if _, err := svc.Login(t.Context(), "ada", testPassword, "198.51.100.7", ""); err != nil {
		t.Errorf("Login() from another address: error = %v", err)
	}
	if _, err := svc.Login(t.Context(), "eve", "wrong password", "203.0.113.9", ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Login() for another user: error = %v, want ErrUnauthorized", err)
	}

	// The window is a minute wide.
	clock.Advance(61 * time.Second)

	if _, err := svc.Login(t.Context(), "ada", testPassword, addr, ""); err != nil {
		t.Errorf("Login() after the window: error = %v", err)
	}
}

func TestALoginSuccessClearsTheRateLimit(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	const addr = "203.0.113.4"
	for range 4 {
		if _, err := svc.Login(t.Context(), "ada", "wrong password", addr, ""); !errors.Is(err, service.ErrUnauthorized) {
			t.Fatalf("error = %v, want ErrUnauthorized", err)
		}
	}
	if _, err := svc.Login(t.Context(), "ada", testPassword, addr, ""); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	// The counter is gone, so a fresh set of attempts is allowed.
	if _, err := svc.Login(t.Context(), "ada", "wrong password", addr, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized rather than ErrQuota", err)
	}
}

func TestRefreshRotatesTheToken(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session := mustLogin(t, svc, "ada")
	rotated, err := svc.Refresh(t.Context(), session.RefreshToken, "curl/9")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	if rotated.RefreshToken == session.RefreshToken {
		t.Error("Refresh() returned the same refresh token")
	}
	if rotated.AccessToken == session.AccessToken {
		t.Error("Refresh() returned the same access token")
	}
	if rotated.User.ID != session.User.ID {
		t.Errorf("User = %+v, want %s", rotated.User, session.User.ID)
	}

	// The new token keeps working.
	if _, err := svc.Refresh(t.Context(), rotated.RefreshToken, "curl/10"); err != nil {
		t.Errorf("Refresh() with the rotated token: error = %v", err)
	}

	// The old one does not.
	if _, err := svc.Refresh(t.Context(), session.RefreshToken, "curl/10"); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() with the spent token: error = %v, want ErrUnauthorized", err)
	}
}

func TestRefreshRejectsAnUnknownToken(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	if _, err := svc.Refresh(t.Context(), "not-a-token", ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() error = %v, want ErrUnauthorized", err)
	}
}

func TestRefreshReuseRevokesTheWholeFamily(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session := mustLogin(t, svc, "ada")
	rotated, err := svc.Refresh(t.Context(), session.RefreshToken, "")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	// Replaying a spent token means it leaked: the successor dies too.
	if _, err := svc.Refresh(t.Context(), session.RefreshToken, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Fatalf("Refresh() error = %v, want ErrUnauthorized", err)
	}
	if _, err := svc.Refresh(t.Context(), rotated.RefreshToken, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() with the successor: error = %v, want ErrUnauthorized after a replay", err)
	}

	// Logging in again works: the family is dead, not the user.
	if _, err := svc.Login(t.Context(), "ada", testPassword, "", ""); err != nil {
		t.Errorf("Login() after a replay: error = %v", err)
	}
}

func TestRefreshRejectsAnExpiredToken(t *testing.T) {
	t.Parallel()

	svc, _, clock := newService(t)
	mustSetup(t, svc, "ada")

	session := mustLogin(t, svc, "ada")

	clock.Advance(testRefreshTTL + time.Second)

	if _, err := svc.Refresh(t.Context(), session.RefreshToken, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() error = %v, want ErrUnauthorized for an expired token", err)
	}

	// It is revoked, so replaying it cannot revive the family either.
	clock.Advance(-(testRefreshTTL + time.Second))
	if _, err := svc.Refresh(t.Context(), session.RefreshToken, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() error = %v, want ErrUnauthorized", err)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session := mustLogin(t, svc, "ada")

	if err := svc.Logout(t.Context(), session.RefreshToken); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := svc.Refresh(t.Context(), session.RefreshToken, ""); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Refresh() after logout: error = %v, want ErrUnauthorized", err)
	}

	// Logging out again, or with a token that never existed, is fine.
	if err := svc.Logout(t.Context(), session.RefreshToken); err != nil {
		t.Errorf("Logout() twice: error = %v", err)
	}
	if err := svc.Logout(t.Context(), "not-a-token"); err != nil {
		t.Errorf("Logout() with an unknown token: error = %v", err)
	}
	if err := svc.Logout(t.Context(), ""); err != nil {
		t.Errorf("Logout() without a token: error = %v", err)
	}
}

func TestLogoutDoesNotTouchTheRestOfTheFamily(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	mustSetup(t, svc, "ada")

	session := mustLogin(t, svc, "ada")
	rotated, err := svc.Refresh(t.Context(), session.RefreshToken, "")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	if err := svc.Logout(t.Context(), session.RefreshToken); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := svc.Refresh(t.Context(), rotated.RefreshToken, ""); err != nil {
		t.Errorf("Refresh() with the live token: error = %v", err)
	}
}

func TestMe(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)
	admin := mustSetup(t, svc, "ada")

	user, err := svc.Me(t.Context(), admin.ID)
	if err != nil {
		t.Fatalf("Me() error = %v", err)
	}
	if user.Username != "ada" || user.Role != kumoauth.RoleAdmin {
		t.Errorf("Me() = %+v, want the admin ada", user)
	}
	if user.CreatedAt.IsZero() {
		t.Error("Me() returned a user without a creation time")
	}

	if _, err := svc.Me(t.Context(), "no-such-user"); !errors.Is(err, service.ErrUnauthorized) {
		t.Errorf("Me() error = %v, want ErrUnauthorized for a deleted user", err)
	}
}

func TestRefreshTokenHashesAreStoredNotTokens(t *testing.T) {
	t.Parallel()

	svc, db, _ := newService(t)
	mustSetup(t, svc, "ada")
	session := mustLogin(t, svc, "ada")

	var stored string
	err := db.DB().QueryRowContext(t.Context(),
		`SELECT token_hash FROM refresh_tokens`).Scan(&stored)
	if err != nil {
		t.Fatalf("read refresh_tokens: %v", err)
	}

	if stored == session.RefreshToken {
		t.Error("the database holds the refresh token itself")
	}
	if stored != kumoauth.HashRefreshToken(session.RefreshToken) {
		t.Error("the database does not hold the hash of the refresh token")
	}
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	t.Parallel()

	full := auth.Options{
		Logger:     logging.Discard(),
		Store:      storetest.New(t),
		Signer:     testSigner(t),
		RefreshTTL: testRefreshTTL,
	}

	tests := []struct {
		name string
		drop func(*auth.Options)
	}{
		{name: "no logger", drop: func(o *auth.Options) { o.Logger = nil }},
		{name: "no store", drop: func(o *auth.Options) { o.Store = nil }},
		{name: "no signer", drop: func(o *auth.Options) { o.Signer = nil }},
		{name: "no refresh lifetime", drop: func(o *auth.Options) { o.RefreshTTL = 0 }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := full
			tt.drop(&opts)

			if _, err := auth.New(opts); err == nil {
				t.Error("New() error = nil, want an error")
			}
		})
	}
}

func TestNeedsSetupOnAnEmptyInstance(t *testing.T) {
	t.Parallel()

	svc, _, _ := newService(t)

	needs, err := svc.NeedsSetup(t.Context())
	if err != nil {
		t.Fatalf("NeedsSetup() error = %v", err)
	}
	if !needs {
		t.Error("NeedsSetup() = false on an instance without users")
	}
}

// clock is the time source of the service under test. Tests move it forward to
// cross the rate-limit window or a token expiry without sleeping.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newService(t *testing.T) (*auth.Service, *store.Store, *clock) {
	t.Helper()

	clk := &clock{now: time.Now().UTC()}
	db := storetest.New(t)

	svc, err := auth.New(auth.Options{
		Logger:     logging.Discard(),
		Store:      db,
		Signer:     testSigner(t),
		RefreshTTL: testRefreshTTL,
		Now:        clk.Now,
	})
	if err != nil {
		t.Fatalf("auth.New(): %v", err)
	}

	return svc, db, clk
}

func mustSetup(t *testing.T, svc *auth.Service, username string) auth.User {
	t.Helper()

	user, err := svc.Setup(context.Background(), username, testPassword)
	if err != nil {
		t.Fatalf("Setup(%q): %v", username, err)
	}
	return user
}

func mustLogin(t *testing.T, svc *auth.Service, username string) auth.Session {
	t.Helper()

	session, err := svc.Login(context.Background(), username, testPassword, "203.0.113.1", "vitest")
	if err != nil {
		t.Fatalf("Login(%q): %v", username, err)
	}
	return session
}

func testSigner(t *testing.T) *kumoauth.Signer {
	t.Helper()

	signer, err := kumoauth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner(): %v", err)
	}
	return signer
}
