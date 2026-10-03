package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

// Check is a single readiness probe.
type Check struct {
	Name  string
	Probe func(ctx context.Context) error
}

// Options configures the HTTP router.
type Options struct {
	// Logger is used for access logs, recovered panics and unhandled errors.
	Logger *slog.Logger
	// GinMode is gin.ReleaseMode, gin.DebugMode or gin.TestMode; it follows
	// KUMO_LOG_LEVEL.
	GinMode string
	// System answers GET /api/v1/system/info.
	System *system.Service
	// Auth answers the /api/v1/auth endpoints.
	Auth *authservice.Service
	// KeyPairs answers the /api/v1/key-pairs endpoints.
	KeyPairs *keypair.Service
	// Signer verifies access tokens and guards every authenticated route.
	Signer *auth.Signer
	// RefreshTTL is the lifetime of the refresh cookie.
	RefreshTTL time.Duration
	// CookieSecure marks the refresh cookie Secure. It must stay true behind
	// TLS and be false only for plain HTTP development.
	CookieSecure bool
	// ReadyChecks are probed by GET /readyz; all of them must pass.
	ReadyChecks []Check
	// AllowedOrigins enables development CORS for the Vite dev server.
	AllowedOrigins []string
	// Console serves the embedded web console. It answers every route that is
	// not an API or health route, so React Router can own its own paths. Nil
	// disables it, which is what the API-only tests do.
	Console http.Handler
}

// Handler owns the routes of the control plane.
type Handler struct {
	logger         *slog.Logger
	system         *system.Service
	auth           *authservice.Service
	keyPairs       *keypair.Service
	signer         *auth.Signer
	refreshTTL     time.Duration
	cookieSecure   bool
	checks         []Check
	allowedOrigins []string
	console        http.Handler
	ginMode        string
}

// NewHandler builds the API handler.
func NewHandler(opts Options) *Handler {
	return &Handler{
		logger:         opts.Logger,
		system:         opts.System,
		auth:           opts.Auth,
		keyPairs:       opts.KeyPairs,
		signer:         opts.Signer,
		refreshTTL:     opts.RefreshTTL,
		cookieSecure:   opts.CookieSecure,
		checks:         opts.ReadyChecks,
		allowedOrigins: opts.AllowedOrigins,
		console:        opts.Console,
		ginMode:        opts.GinMode,
	}
}

// Router returns the fully wired gin engine: middleware, routes and the JSON
// fallbacks for unknown routes and methods.
func (h *Handler) Router() *gin.Engine {
	gin.SetMode(h.mode())

	engine := gin.New()
	engine.HandleMethodNotAllowed = true

	engine.Use(
		middleware.RequestID(),
		middleware.Recovery(h.logger),
		middleware.AccessLog(h.logger),
		middleware.CORS(h.allowedOrigins),
	)

	engine.GET("/healthz", h.healthz)
	engine.GET("/readyz", h.readyz)

	v1 := engine.Group("/api/v1")
	v1.GET("/system/info", h.systemInfo)

	// The auth endpoints carry the refresh cookie instead of an access token,
	// so they sit outside the authenticated group.
	authRoutes := v1.Group("/auth")
	authRoutes.POST("/setup", h.authSetup)
	authRoutes.POST("/login", h.authLogin)
	authRoutes.POST("/refresh", h.authRefresh)
	authRoutes.POST("/logout", h.authLogout)

	// Everything below needs a valid access token.
	secured := v1.Group("", middleware.RequireAuth(h.signer))
	secured.GET("/auth/me", h.authMe)

	// Key pairs. Reads are open to both roles; writes are admin only
	// (docs/design/01-api.md §Authorization matrix).
	if h.keyPairs != nil {
		secured.GET("/key-pairs", h.listKeyPairs)
		secured.GET("/key-pairs/:id", h.getKeyPair)
		write := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/key-pairs", h.createKeyPair)
		write.POST("/key-pairs/import", h.importKeyPair)
		write.PATCH("/key-pairs/:id", h.updateKeyPair)
		write.DELETE("/key-pairs/:id", h.deleteKeyPair)
	}

	engine.NoRoute(h.notFound)
	engine.NoMethod(func(c *gin.Context) {
		WriteError(c, h.logger, methodNotAllowed(c))
	})

	return engine
}

// notFound answers unknown routes. API paths keep the JSON error envelope even
// once the console is mounted, because a client calling the API must not be
// handed HTML; everything else is a console route.
func (h *Handler) notFound(c *gin.Context) {
	if h.console != nil && !isAPIPath(c.Request.URL.Path) {
		h.console.ServeHTTP(c.Writer, c.Request)
		return
	}

	WriteError(c, h.logger, notFound(c))
}

// isAPIPath reports whether a path belongs to the API. Health checks live
// outside /api/v1 but are registered routes, so they never reach here.
func isAPIPath(urlPath string) bool {
	return urlPath == "/api" || strings.HasPrefix(urlPath, "/api/")
}

func (h *Handler) mode() string {
	switch h.ginMode {
	case gin.DebugMode, gin.TestMode, gin.ReleaseMode:
		return h.ginMode
	default:
		return gin.ReleaseMode
	}
}

type healthResponse struct {
	Status string `json:"status"`
}

// healthz is the liveness probe: it answers while the process runs.
func (h *Handler) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, healthResponse{Status: "ok"})
}

type readyResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// readyz is the readiness probe: every dependency (database, hypervisor) must
// answer. Details of a failure are logged, never returned, because the endpoint
// needs no authentication.
func (h *Handler) readyz(c *gin.Context) {
	ctx := c.Request.Context()
	checks := make(map[string]string, len(h.checks))
	ready := true

	for _, check := range h.checks {
		if err := check.Probe(ctx); err != nil {
			h.logger.ErrorContext(ctx, "readiness check failed",
				"request_id", middleware.RequestIDFrom(c),
				"check", check.Name,
				"error", err,
			)
			checks[check.Name] = "failed"
			ready = false
			continue
		}
		checks[check.Name] = "ok"
	}

	status, state := http.StatusOK, "ok"
	if !ready {
		status, state = http.StatusServiceUnavailable, "unavailable"
	}

	c.JSON(status, readyResponse{Status: state, Checks: checks})
}

// systemInfo reports the build, whether first-run setup is still needed and the
// hypervisor status. It is the one /api/v1 route that needs no access token.
func (h *Handler) systemInfo(c *gin.Context) {
	info, err := h.system.Info(c.Request.Context())
	if err != nil {
		WriteError(c, h.logger, err)
		return
	}

	c.JSON(http.StatusOK, info)
}
