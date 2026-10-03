package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/auth"
)

// ContextKeyPrincipal is the gin context key holding the authenticated caller.
const ContextKeyPrincipal = "principal"

// BearerPrefix is the Authorization scheme of an access token.
const BearerPrefix = "Bearer "

// PrincipalFrom returns the authenticated caller of the current request, or the
// zero value when the request was not authenticated.
func PrincipalFrom(c *gin.Context) auth.Principal {
	value, ok := c.Get(ContextKeyPrincipal)
	if !ok {
		return auth.Principal{}
	}
	principal, ok := value.(auth.Principal)
	if !ok {
		return auth.Principal{}
	}
	return principal
}

// RequireAuth rejects a request without a valid access token. It answers 401
// for a missing, malformed, forged and expired token alike, so it cannot be
// used to probe for valid ones.
func RequireAuth(signer *auth.Signer) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := bearerToken(c)
		if !ok {
			abortUnauthorized(c)
			return
		}

		principal, err := signer.Verify(token)
		if err != nil {
			abortUnauthorized(c)
			return
		}

		c.Set(ContextKeyPrincipal, principal)
		c.Next()
	}
}

// RequireRole rejects a caller whose role is not the one asked for. Read routes
// use RequireAuth alone; write routes add RequireRole(auth.RoleAdmin), which
// answers 403 to a viewer (docs/design/01-api.md).
func RequireRole(role auth.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		if PrincipalFrom(c).Role != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": gin.H{
					"code":       "permission_denied",
					"message":    "this action requires the " + string(role) + " role",
					"request_id": RequestIDFrom(c),
				},
			})
			return
		}

		c.Next()
	}
}

// bearerToken extracts the token from the Authorization header.
func bearerToken(c *gin.Context) (string, bool) {
	header := c.GetHeader("Authorization")
	if !strings.HasPrefix(header, BearerPrefix) {
		return "", false
	}

	token := strings.TrimSpace(strings.TrimPrefix(header, BearerPrefix))
	if token == "" {
		return "", false
	}

	return token, true
}

// abortUnauthorized ends the chain with the error envelope of docs/design/01-api.md.
func abortUnauthorized(c *gin.Context) {
	c.Header("WWW-Authenticate", `Bearer realm="kumo"`)
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"error": gin.H{
			"code":       "unauthenticated",
			"message":    "a valid access token is required",
			"request_id": RequestIDFrom(c),
		},
	})
}
