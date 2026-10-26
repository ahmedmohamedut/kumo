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
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/service/vpc"
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
	// Images answers the /api/v1/images endpoints.
	Images *image.Service
	// InstanceTypes answers the /api/v1/instance-types endpoints.
	InstanceTypes *instancetype.Service
	// Instances answers the /api/v1/instances endpoints.
	Instances *instance.Service
	// VPCs answers the /api/v1/vpcs endpoints.
	VPCs *vpc.Service
	// Subnets answers the /api/v1/subnets endpoints.
	Subnets *subnet.Service
	// Buckets answers the /api/v1/s3/buckets endpoints. Nil means object
	// storage is off, and every /s3 route answers 503.
	Buckets *bucket.Service
	// Tasks answers the /api/v1/tasks endpoints, which is where the Location
	// header of an asynchronous image operation points.
	Tasks *task.Service
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
	images         *image.Service
	instanceTypes  *instancetype.Service
	instances      *instance.Service
	vpcs           *vpc.Service
	subnets        *subnet.Service
	buckets        *bucket.Service
	tasks          *task.Service
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
		images:         opts.Images,
		instanceTypes:  opts.InstanceTypes,
		instances:      opts.Instances,
		vpcs:           opts.VPCs,
		subnets:        opts.Subnets,
		buckets:        opts.Buckets,
		tasks:          opts.Tasks,
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

	// Images. Reads are open to both roles; imports, patches and deletions are
	// admin only, because they change the pool every instance boots from.
	if h.images != nil {
		secured.GET("/images", h.listImages)
		// The catalog is a static route and must win over /images/:id.
		secured.GET("/images/catalog", h.listImageCatalog)
		secured.GET("/images/:id", h.getImage)
		write := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/images/import", h.importImage)
		write.POST("/images/:id/retry", h.retryImageImport)
		write.PATCH("/images/:id", h.updateImage)
		write.DELETE("/images/:id", h.deleteImage)
	}

	// Instance types. Reads are open to both roles, because a viewer picks one in
	// the launch form; managing the set is admin only.
	if h.instanceTypes != nil {
		secured.GET("/instance-types", h.listInstanceTypes)
		secured.GET("/instance-types/:id", h.getInstanceType)
		write := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/instance-types", h.createInstanceType)
		write.PATCH("/instance-types/:id", h.updateInstanceType)
		write.DELETE("/instance-types/:id", h.deleteInstanceType)
	}

	// Instances. Reads are open to both roles; launching is admin only, because it
	// claims the vCPUs, memory and disk of the node (docs/design/01-api.md
	// §Authorization matrix).
	if h.instances != nil {
		secured.GET("/instances", h.listInstances)
		secured.GET("/instances/:id", h.getInstance)

		// Everything below needs an administrator. Launching and the lifecycle
		// actions change what runs on the node, and the console output is the boot
		// log of a guest, which carries whatever the cloud-init of that instance
		// printed into it (docs/design/01-api.md §Instances).
		admin := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		admin.POST("/instances", h.launchInstance)
		admin.PATCH("/instances/:id", h.updateInstance)
		admin.DELETE("/instances/:id", h.terminateInstance)
		admin.POST("/instances/:id/start", h.startInstance)
		admin.POST("/instances/:id/stop", h.stopInstance)
		admin.POST("/instances/:id/reboot", h.rebootInstance)
		admin.GET("/instances/:id/console-output", h.instanceConsoleOutput)
		admin.GET("/instances/:id/console", h.openInstanceConsole)
	}

	// VPCs. Reads are open to both roles; creating and deleting change the
	// network an operator's hosts live on, so the writes are admin only
	// (docs/design/01-api.md §VPCs).
	if h.vpcs != nil {
		secured.GET("/vpcs", h.listVPCs)
		secured.GET("/vpcs/:id", h.getVPC)
		write := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/vpcs", h.createVPC)
		write.PATCH("/vpcs/:id", h.updateVPC)
		write.DELETE("/vpcs/:id", h.deleteVPC)
	}

	// Subnets. Reads are open to both roles; the writes are admin only, like
	// the VPCs they partition (docs/design/01-api.md §Subnets).
	if h.subnets != nil {
		secured.GET("/subnets", h.listSubnets)
		secured.GET("/subnets/:id", h.getSubnet)
		write := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/subnets", h.createSubnet)
		write.PATCH("/subnets/:id", h.updateSubnet)
		write.DELETE("/subnets/:id", h.deleteSubnet)
	}

	// Object storage (docs/design/06-object-storage.md). The routes exist on
	// every node, so a node with the module off answers 503 and says why
	// instead of a 404 that reads like a typo. Reads are open to both roles;
	// writes are admin only.
	s3 := secured.Group("/s3")
	if h.buckets == nil {
		s3.Any("/*path", h.objectStorageOff)
	} else {
		s3.GET("/buckets", h.listBuckets)
		s3.GET("/buckets/:name", h.getBucket)
		s3.GET("/buckets/:name/objects", h.listObjects)
		// A download is a read, so a viewer may have its URL.
		s3.POST("/buckets/:name/downloads", h.presignDownload)
		write := s3.Group("", middleware.RequireRole(auth.RoleAdmin))
		write.POST("/buckets", h.createBucket)
		write.PATCH("/buckets/:name", h.updateBucket)
		write.DELETE("/buckets/:name", h.deleteBucket)
		write.POST("/buckets/:name/uploads", h.presignUpload)
		write.POST("/buckets/:name/folders", h.createFolder)
		write.POST("/buckets/:name/objects/delete", h.deleteObjects)
	}

	// Host capacity. A launch form shows it next to the instance type, so both
	// roles read it.
	if h.system != nil {
		secured.GET("/system/capacity", h.systemCapacity)
	}

	// The reconciler's orphan report: domains running under an instance id that
	// no row claims (docs/design/00-architecture.md §4.3). Admin-only, because
	// it describes the host rather than anything Kumo manages.
	if h.instances != nil {
		orphans := secured.Group("", middleware.RequireRole(auth.RoleAdmin))
		orphans.GET("/system/orphans", h.systemOrphans)
	}

	// Tasks are readable by both roles: a viewer may watch an image import that
	// an admin started.
	if h.tasks != nil {
		secured.GET("/tasks", h.listTasks)
		secured.GET("/tasks/:id", h.getTask)
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
