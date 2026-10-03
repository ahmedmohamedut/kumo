package api

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/service"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
)

// refreshCookieName is the cookie carrying the refresh token.
const refreshCookieName = "refresh_token"

// refreshCookiePath keeps the cookie off every other route: only the auth
// endpoints need it, and a narrower path shrinks the blast radius of an XSS.
const refreshCookiePath = "/api/v1/auth"

// credentialsRequest is the body of POST /auth/setup and POST /auth/login.
type credentialsRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// tokenResponse is the body of a successful login or refresh.
type tokenResponse struct {
	AccessToken string            `json:"access_token"`
	ExpiresIn   int               `json:"expires_in"`
	User        *authservice.User `json:"user,omitempty"`
}

// authSetup creates the first administrator. It answers 409 once the instance
// has a user, whatever token the caller presents.
func (h *Handler) authSetup(c *gin.Context) {
	var req credentialsRequest
	if !h.bindCredentials(c, &req) {
		return
	}

	user, err := h.auth.Setup(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusCreated, user)
}

// authLogin verifies a password and sets the refresh cookie.
func (h *Handler) authLogin(c *gin.Context) {
	var req credentialsRequest
	if !h.bindCredentials(c, &req) {
		return
	}

	session, err := h.auth.Login(c.Request.Context(), req.Username, req.Password,
		c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	h.setRefreshCookie(c, session.RefreshToken)
	c.JSON(http.StatusOK, tokenResponse{
		AccessToken: session.AccessToken,
		ExpiresIn:   int(session.AccessExpiry.Seconds()),
		User:        &session.User,
	})
}

// authRefresh rotates the refresh cookie and issues a new access token.
func (h *Handler) authRefresh(c *gin.Context) {
	token, ok := h.refreshToken(c)
	if !ok {
		return
	}

	session, err := h.auth.Refresh(c.Request.Context(), token, c.Request.UserAgent())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	h.setRefreshCookie(c, session.RefreshToken)
	c.JSON(http.StatusOK, tokenResponse{
		AccessToken: session.AccessToken,
		ExpiresIn:   int(session.AccessExpiry.Seconds()),
	})
}

// authLogout revokes the refresh token and clears the cookie.
func (h *Handler) authLogout(c *gin.Context) {
	token, ok := h.refreshToken(c)
	if !ok {
		return
	}

	if err := h.auth.Logout(c.Request.Context(), token); err != nil {
		WriteError(c, h.logger, err)
		return
	}

	h.clearRefreshCookie(c)
	c.Status(http.StatusNoContent)
}

// authMe returns the user behind the access token.
func (h *Handler) authMe(c *gin.Context) {
	principal := middleware.PrincipalFrom(c)

	user, err := h.auth.Me(c.Request.Context(), principal.UserID)
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, user)
}

// bindCredentials decodes and checks the credentials body. Only a malformed or
// empty body is rejected here: whether the credentials are correct is the
// service's answer, so login cannot be used to find out what exists.
func (h *Handler) bindCredentials(c *gin.Context, req *credentialsRequest) bool {
	if err := c.ShouldBindJSON(req); err != nil {
		WriteError(c, h.logger, Invalid("the request body is not a valid JSON object",
			FieldError{Field: "body", Reason: "must be a JSON object with username and password"}))
		return false
	}

	var fields []FieldError
	if req.Username == "" {
		fields = append(fields, FieldError{Field: "username", Reason: "must not be empty"})
	}
	if req.Password == "" {
		fields = append(fields, FieldError{Field: "password", Reason: "must not be empty"})
	}
	if len(fields) > 0 {
		WriteError(c, h.logger, Invalid("username and password are required", fields...))
		return false
	}

	return true
}

// refreshToken returns the refresh cookie, or answers 401 when it is missing:
// without it there is nothing to rotate.
func (h *Handler) refreshToken(c *gin.Context) (string, bool) {
	token, err := c.Cookie(refreshCookieName)
	if err != nil || token == "" {
		WriteError(c, h.logger, &statusError{
			status: http.StatusUnauthorized,
			code:   CodeUnauthenticated,
			err:    fmt.Errorf("the refresh cookie is missing: %w", service.ErrUnauthorized),
		})
		return "", false
	}

	return token, true
}

// setRefreshCookie stores the refresh token HttpOnly, so script running in the
// console cannot read it, and SameSite=Strict, so it never rides along on a
// cross-site request.
func (h *Handler) setRefreshCookie(c *gin.Context, token string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshCookieName,
		Value:    token,
		Path:     refreshCookiePath,
		MaxAge:   int(h.refreshTTL.Seconds()),
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearRefreshCookie expires the cookie in the browser.
func (h *Handler) clearRefreshCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     refreshCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   h.cookieSecure,
		SameSite: http.SameSiteStrictMode,
	})
}
