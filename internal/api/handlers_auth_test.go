package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

const (
	testPassword = "correct horse battery staple"
	testUsername = "ada"
)

type userBody struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

type tokenBody struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   int       `json:"expires_in"`
	User        *userBody `json:"user"`
}

func TestAuthSetup(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/setup", `{"username":"ada","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /auth/setup status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}

	var user userBody
	decode(t, recorder, &user)
	if user.Username != testUsername || user.Role != "admin" {
		t.Errorf("body = %+v, want the admin ada", user)
	}
	if user.ID == "" || user.CreatedAt.IsZero() {
		t.Errorf("body = %+v, want an id and a creation time", user)
	}

	// A second setup is refused.
	recorder = do(t, router, http.MethodPost, "/api/v1/auth/setup", `{"username":"eve","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusConflict {
		t.Errorf("POST /auth/setup status = %d, want 409 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeConflict)
}

func TestAuthSetupValidates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "a short password", body: `{"username":"ada","password":"short"}`, want: "password"},
		{name: "a short username", body: `{"username":"ab","password":"` + testPassword + `"}`, want: "username"},
		{name: "a missing password", body: `{"username":"ada"}`, want: "password"},
		{name: "an empty username", body: `{"username":"","password":"` + testPassword + `"}`, want: "username"},
		{name: "not JSON", body: `{"username":`, want: "body"},
		{name: "not an object", body: `["ada"]`, want: "body"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newAuthRouter(t)

			recorder := do(t, router, http.MethodPost, "/api/v1/auth/setup", tt.body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", recorder.Code, recorder.Body)
			}

			var envelope errorEnvelope
			decode(t, recorder, &envelope)
			if envelope.Error.Code != api.CodeInvalidArgument {
				t.Errorf("code = %q, want %q", envelope.Error.Code, api.CodeInvalidArgument)
			}

			var found bool
			for _, detail := range envelope.Error.Details {
				if detail.Field == tt.want {
					found = true
				}
			}
			if !found {
				t.Errorf("details = %+v, want an error on %q", envelope.Error.Details, tt.want)
			}
		})
	}
}

func TestAuthLogin(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)
	setupUser(t, router)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/login",
		`{"username":"ada","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /auth/login status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body tokenBody
	decode(t, recorder, &body)
	if body.AccessToken == "" {
		t.Error("no access token in the response")
	}
	if body.ExpiresIn != 900 {
		t.Errorf("expires_in = %d, want 900", body.ExpiresIn)
	}
	if body.User == nil || body.User.Username != testUsername {
		t.Errorf("user = %+v, want ada", body.User)
	}

	cookie := refreshCookie(t, recorder.Result())
	if cookie.Value == "" {
		t.Error("the refresh cookie is empty")
	}
	if !cookie.HttpOnly {
		t.Error("the refresh cookie is not HttpOnly, so script could read it")
	}
	if cookie.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", cookie.SameSite)
	}
	if cookie.Path != "/api/v1/auth" {
		t.Errorf("Path = %q, want /api/v1/auth", cookie.Path)
	}
	if cookie.MaxAge != int((7 * 24 * time.Hour).Seconds()) {
		t.Errorf("MaxAge = %d, want the refresh lifetime", cookie.MaxAge)
	}
	if cookie.Secure {
		t.Error("Secure is set although the test server runs on plain HTTP")
	}
}

func TestAuthLoginMarksTheCookieSecure(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouterWith(t, true)
	setupUser(t, router)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/login",
		`{"username":"ada","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /auth/login status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	if cookie := refreshCookie(t, recorder.Result()); !cookie.Secure {
		t.Error("the refresh cookie is not Secure behind TLS")
	}
}

func TestAuthLoginRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "a wrong password",
			body: `{"username":"ada","password":"wrong password"}`,
			want: http.StatusUnauthorized,
		},
		{
			name: "an unknown user",
			body: `{"username":"eve","password":"` + testPassword + `"}`,
			want: http.StatusUnauthorized,
		},
		{
			name: "no password",
			body: `{"username":"ada"}`,
			want: http.StatusBadRequest,
		},
		{
			name: "not JSON",
			body: `{`,
			want: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newAuthRouter(t)
			setupUser(t, router)

			recorder := do(t, router, http.MethodPost, "/api/v1/auth/login", tt.body)
			if recorder.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.want, recorder.Body)
			}

			if tt.want == http.StatusUnauthorized {
				assertErrorCode(t, recorder, api.CodeUnauthenticated)
				if strings.Contains(recorder.Body.String(), "ada") {
					t.Errorf("body = %q, want no hint about the username", recorder.Body)
				}
			}
		})
	}
}

func TestAuthLoginIsRateLimited(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)
	setupUser(t, router)

	body := `{"username":"ada","password":"wrong password"}`
	for attempt := range 5 {
		recorder := do(t, router, http.MethodPost, "/api/v1/auth/login", body)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401", attempt+1, recorder.Code)
		}
	}

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/login", body)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeQuotaExceeded)
}

func TestAuthRefresh(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithCookie(t, router, http.MethodPost, "/api/v1/auth/refresh", "", login.refresh)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /auth/refresh status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body tokenBody
	decode(t, recorder, &body)
	if body.AccessToken == "" || body.ExpiresIn != 900 {
		t.Errorf("body = %+v, want a fresh access token", body)
	}
	if body.User != nil {
		t.Error("the refresh response carries a user, the schema does not define one")
	}

	rotated := refreshCookie(t, recorder.Result())
	if rotated.Value == "" || rotated.Value == login.refresh {
		t.Error("the refresh cookie was not rotated")
	}

	// The new cookie works, the spent one does not.
	if recorder = doWithCookie(t, router, http.MethodPost, "/api/v1/auth/refresh", "", rotated.Value); recorder.Code != http.StatusOK {
		t.Errorf("refreshing with the rotated cookie: status = %d, want 200", recorder.Code)
	}

	recorder = doWithCookie(t, router, http.MethodPost, "/api/v1/auth/refresh", "", login.refresh)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("refreshing with the spent cookie: status = %d, want 401", recorder.Code)
	}
	assertErrorCode(t, recorder, api.CodeUnauthenticated)
}

func TestAuthRefreshWithoutCookie(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/refresh", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeUnauthenticated)
}

func TestAuthRefreshWithAnUnknownCookie(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)

	recorder := doWithCookie(t, router, http.MethodPost, "/api/v1/auth/refresh", "", "made-up-token")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body)
	}
}

func TestAuthLogout(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithCookie(t, router, http.MethodPost, "/api/v1/auth/logout", "", login.refresh)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("POST /auth/logout status = %d, want 204 (body %s)", recorder.Code, recorder.Body)
	}

	cleared := refreshCookie(t, recorder.Result())
	if cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want it emptied and expired", cleared)
	}

	recorder = doWithCookie(t, router, http.MethodPost, "/api/v1/auth/refresh", "", login.refresh)
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("refreshing after logout: status = %d, want 401", recorder.Code)
	}
}

func TestAuthLogoutWithoutCookie(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/logout", "")
	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", recorder.Code)
	}
}

func TestAuthMe(t *testing.T) {
	t.Parallel()

	router, _ := newAuthRouter(t)
	login := setupUserAndLogin(t, router)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/auth/me", "", login.access)
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /auth/me status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var user userBody
	decode(t, recorder, &user)
	if user.Username != testUsername || user.Role != "admin" {
		t.Errorf("body = %+v, want the admin ada", user)
	}
}

func TestAuthMeRejectsBadTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
	}{
		{name: "no token"},
		{name: "an empty bearer", token: middlewareBearer},
		{name: "a token that is not a JWT", token: "not-a-jwt"},
		{name: "an unsigned token", token: "eyJhbGciOiJub25lIn0." +
			"eyJzdWIiOiJ1c2VyLTEiLCJyb2xlIjoiYWRtaW4iLCJleHAiOjQ4ODM2NDI0MDB9."},
		{name: "a token signed with another key", token: foreignToken(t)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			router, _ := newAuthRouter(t)
			setupUser(t, router)

			recorder := doWithToken(t, router, http.MethodGet, "/api/v1/auth/me", "", tt.token)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", recorder.Code, recorder.Body)
			}
			assertErrorCode(t, recorder, api.CodeUnauthenticated)
			if recorder.Body.String() == "" || !strings.Contains(recorder.Body.String(), "{") {
				t.Error("the response is not the JSON error envelope")
			}
		})
	}
}

func TestAuthMeRejectsAnExpiredToken(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }

	router, _ := newAuthRouterWithClock(t, clock)
	login := setupUserAndLogin(t, router)

	if recorder := doWithToken(t, router, http.MethodGet, "/api/v1/auth/me", "", login.access); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 before the expiry (body %s)", recorder.Code, recorder.Body)
	}

	now = now.Add(16 * time.Minute)

	recorder := doWithToken(t, router, http.MethodGet, "/api/v1/auth/me", "", login.access)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 after the expiry (body %s)", recorder.Code, recorder.Body)
	}
	assertErrorCode(t, recorder, api.CodeUnauthenticated)
}

const middlewareBearer = "Bearer "

func newAuthRouter(t *testing.T) (*gin.Engine, *auth.Signer) {
	t.Helper()
	return newAuthRouterWith(t, false)
}

func newAuthRouterWith(t *testing.T, cookieSecure bool) (*gin.Engine, *auth.Signer) {
	t.Helper()
	return buildAuthRouter(t, cookieSecure, time.Now)
}

func newAuthRouterWithClock(t *testing.T, clock func() time.Time) (*gin.Engine, *auth.Signer) {
	t.Helper()
	return buildAuthRouter(t, false, clock)
}

func buildAuthRouter(t *testing.T, cookieSecure bool, clock func() time.Time) (*gin.Engine, *auth.Signer) {
	t.Helper()

	logger := logging.Discard()
	signer, err := auth.NewSigner([]byte(strings.Repeat("k", 32)), 15*time.Minute,
		auth.WithClock(clock))
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}

	svc, err := authservice.New(authservice.Options{
		Logger:     logger,
		Store:      storetest.New(t),
		Signer:     signer,
		RefreshTTL: 7 * 24 * time.Hour,
		Now:        clock,
	})
	if err != nil {
		t.Fatalf("authservice.New(): %v", err)
	}

	handler := api.NewHandler(api.Options{
		Logger:       logger,
		GinMode:      gin.TestMode,
		System:       system.New(logger, "test", "fake", nil, nil, nil),
		Auth:         svc,
		Signer:       signer,
		RefreshTTL:   7 * 24 * time.Hour,
		CookieSecure: cookieSecure,
	})

	return handler.Router(), signer
}

func setupUser(t *testing.T, router *gin.Engine) userBody {
	t.Helper()

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/setup",
		`{"username":"ada","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("POST /auth/setup status = %d, want 201 (body %s)", recorder.Code, recorder.Body)
	}

	var user userBody
	decode(t, recorder, &user)
	return user
}

// session holds the two tokens a login hands out.
type session struct {
	access  string
	refresh string
}

func setupUserAndLogin(t *testing.T, router *gin.Engine) session {
	t.Helper()

	setupUser(t, router)

	recorder := do(t, router, http.MethodPost, "/api/v1/auth/login",
		`{"username":"ada","password":"`+testPassword+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("POST /auth/login status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body tokenBody
	decode(t, recorder, &body)

	return session{access: body.AccessToken, refresh: refreshCookie(t, recorder.Result()).Value}
}

func do(t *testing.T, router *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doWithToken(t, router, method, path, body, "")
}

func doWithToken(
	t *testing.T,
	router *gin.Engine,
	method, path, body, token string,
) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "192.0.2.10:4242"
	if token != "" {
		request.Header.Set("Authorization", middlewareBearer+token)
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func doWithCookie(
	t *testing.T,
	router *gin.Engine,
	method, path, body, token string,
) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader = http.NoBody
	if body != "" {
		reader = strings.NewReader(body)
	}

	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.AddCookie(&http.Cookie{Name: "refresh_token", Value: token})
	}

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func refreshCookie(t *testing.T, response *http.Response) *http.Cookie {
	t.Helper()

	for _, cookie := range response.Cookies() {
		if cookie.Name == "refresh_token" {
			return cookie
		}
	}

	t.Fatal("no refresh_token cookie in the response")
	return nil
}

func decode(t *testing.T, recorder *httptest.ResponseRecorder, into any) {
	t.Helper()

	if err := json.Unmarshal(recorder.Body.Bytes(), into); err != nil {
		t.Fatalf("decode %q: %v", recorder.Body.String(), err)
	}
}

func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, want string) {
	t.Helper()

	var envelope errorEnvelope
	decode(t, recorder, &envelope)

	if envelope.Error.Code != want {
		t.Errorf("code = %q, want %q (body %s)", envelope.Error.Code, want, recorder.Body)
	}
}

func foreignToken(t *testing.T) string {
	t.Helper()

	other, err := auth.NewSigner([]byte(strings.Repeat("z", 32)), 15*time.Minute)
	if err != nil {
		t.Fatalf("auth.NewSigner(): %v", err)
	}

	issued, err := other.IssueAccessToken("01J00000000000000000000000", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	return issued.Token
}
