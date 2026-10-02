package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api/middleware"
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
	// ReadyChecks are probed by GET /readyz; all of them must pass.
	ReadyChecks []Check
	// AllowedOrigins enables development CORS for the Vite dev server.
	AllowedOrigins []string
}

// Handler owns the routes of the control plane.
type Handler struct {
	logger         *slog.Logger
	system         *system.Service
	checks         []Check
	allowedOrigins []string
	ginMode        string
}

// NewHandler builds the API handler.
func NewHandler(opts Options) *Handler {
	return &Handler{
		logger:         opts.Logger,
		system:         opts.System,
		checks:         opts.ReadyChecks,
		allowedOrigins: opts.AllowedOrigins,
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

	engine.NoRoute(func(c *gin.Context) {
		WriteError(c, h.logger, notFound(c))
	})
	engine.NoMethod(func(c *gin.Context) {
		WriteError(c, h.logger, methodNotAllowed(c))
	})

	return engine
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
