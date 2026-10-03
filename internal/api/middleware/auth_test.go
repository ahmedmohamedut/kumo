package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/auth"
)

const (
	testSecret = "kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"
	noToken    = ""
)

func TestRequireAuth(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)
	admin := issue(t, signer, "user-1", auth.RoleAdmin)

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "a valid token", header: "Bearer " + admin, wantStatus: http.StatusOK},
		{name: "no header", header: noToken, wantStatus: http.StatusUnauthorized},
		{name: "the wrong scheme", header: admin, wantStatus: http.StatusUnauthorized},
		{name: "a lowercase scheme", header: "bearer " + admin, wantStatus: http.StatusUnauthorized},
		{name: "an empty bearer", header: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "a scheme with no token", header: "Bearer    ", wantStatus: http.StatusUnauthorized},
		{name: "garbage", header: "Bearer not-a-jwt", wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder, _ := call(t, signer, tt.header)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body)
			}
		})
	}
}

func TestRequireAuthPassesThePrincipal(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)
	token := issue(t, signer, "user-1", auth.RoleViewer)

	engine := newEngine(t)
	engine.GET("/me",
		middleware.RequireAuth(signer),
		func(c *gin.Context) {
			principal := middleware.PrincipalFrom(c)
			c.JSON(http.StatusOK, gin.H{"user_id": principal.UserID, "role": string(principal.Role)})
		},
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/me", http.NoBody)
	request.Header.Set("Authorization", "Bearer "+token)
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body)
	}

	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.UserID != "user-1" || body.Role != string(auth.RoleViewer) {
		t.Errorf("body = %+v, want user-1/viewer", body)
	}
}

func TestPrincipalFromWithoutAuthentication(t *testing.T) {
	t.Parallel()

	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	if principal := middleware.PrincipalFrom(context); principal != (auth.Principal{}) {
		t.Errorf("PrincipalFrom() = %+v, want the zero value", principal)
	}
}

func TestRequireRole(t *testing.T) {
	t.Parallel()

	signer := testSigner(t)

	tests := []struct {
		name       string
		role       auth.Role
		wantStatus int
	}{
		{name: "an admin", role: auth.RoleAdmin, wantStatus: http.StatusCreated},
		{name: "a viewer", role: auth.RoleViewer, wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			engine := newEngine(t)
			engine.POST("/instances",
				middleware.RequireAuth(signer),
				middleware.RequireRole(auth.RoleAdmin),
				func(c *gin.Context) { c.Status(http.StatusCreated) },
			)

			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/instances", http.NoBody)
			request.Header.Set("Authorization", "Bearer "+issue(t, signer, "user-1", tt.role))
			engine.ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body)
			}
		})
	}
}

func TestRequireRoleIsUnreachableWithoutAToken(t *testing.T) {
	t.Parallel()

	engine := newEngine(t)
	engine.GET("/instances",
		middleware.RequireAuth(testSigner(t)),
		middleware.RequireRole(auth.RoleAdmin),
		func(c *gin.Context) { c.Status(http.StatusOK) },
	)

	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/instances", http.NoBody))

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401, so an unauthenticated caller never reaches the role check",
			recorder.Code)
	}
}

func TestUnauthorizedCarriesTheEnvelope(t *testing.T) {
	t.Parallel()

	recorder, response := call(t, testSigner(t), noToken)

	if got := response.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}

	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if envelope.Error.Code != "unauthenticated" {
		t.Errorf("code = %q, want unauthenticated", envelope.Error.Code)
	}
	if envelope.Error.Message == "" || envelope.Error.RequestID == "" {
		t.Errorf("envelope = %+v, want a message and a request id", envelope.Error)
	}
}

func newEngine(t *testing.T) *gin.Engine {
	t.Helper()

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.RequestID())
	return engine
}

func call(t *testing.T, signer *auth.Signer, header string) (*httptest.ResponseRecorder, *http.Response) {
	t.Helper()

	engine := newEngine(t)
	engine.GET("/me",
		middleware.RequireAuth(signer),
		func(c *gin.Context) { c.Status(http.StatusOK) },
	)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/me", http.NoBody)
	if header != noToken {
		request.Header.Set("Authorization", header)
	}
	engine.ServeHTTP(recorder, request)

	response := recorder.Result()
	return recorder, response
}

func testSigner(t *testing.T) *auth.Signer {
	t.Helper()

	signer, err := auth.NewSigner([]byte(testSecret), 15*time.Minute)
	if err != nil {
		t.Fatalf("NewSigner(): %v", err)
	}
	return signer
}

func issue(t *testing.T, signer *auth.Signer, userID string, role auth.Role) string {
	t.Helper()

	issued, err := signer.IssueAccessToken(userID, role)
	if err != nil {
		t.Fatalf("IssueAccessToken(): %v", err)
	}
	return issued.Token
}
