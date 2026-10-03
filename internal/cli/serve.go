package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/version"
	"github.com/ahmedmohamedut/kumo/internal/webui"
)

// fakeSubnet is the private range the in-memory backend hands out addresses
// from. It matches the guest network 04-libvirt-integration.md §3 documents.
const fakeSubnet = "192.168.100.0/24"

// serve runs the control plane: HTTP API, task worker and embedded console. It
// is what `kumod` and `kumod serve` do.
func serve(ctx context.Context, stdout io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(cfg.LogLevel, stdout)
	slog.SetDefault(logger)

	logger.InfoContext(ctx, "starting kumod",
		"version", version.Version,
		"commit", version.Commit,
		"listen", cfg.Listen,
		"hypervisor", cfg.Hypervisor,
		"data_dir", cfg.DataDir,
		"db_path", cfg.DBPath,
	)

	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // the process is shutting down anyway

	hv, err := newHypervisor(cfg)
	if err != nil {
		return err
	}
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool:    cfg.PoolImages,
		InstancesPool: cfg.PoolInstances,
		Network:       cfg.Network,
		DataDir:       cfg.DataDir,
	}); err != nil {
		return fmt.Errorf("bootstrap hypervisor infrastructure: %w", err)
	}

	systemService := system.New(logger, version.Version, cfg.Hypervisor, db, hv)

	signer, err := auth.NewSignerFromFile(cfg.JWTSecretFile, cfg.AccessTTL)
	if err != nil {
		return fmt.Errorf("load the JWT signing key: %w", err)
	}
	authService, err := authservice.New(authservice.Options{
		Logger:     logger,
		Store:      db,
		Signer:     signer,
		RefreshTTL: cfg.RefreshTTL,
	})
	if err != nil {
		return fmt.Errorf("build the auth service: %w", err)
	}
	logger.InfoContext(ctx, "auth is ready",
		"secret_file", cfg.JWTSecretFile, "refresh_ttl", cfg.RefreshTTL)

	keyPairs, err := keypair.New(keypair.Options{Logger: logger, Store: db})
	if err != nil {
		return fmt.Errorf("build the key pair service: %w", err)
	}

	console, err := webui.New(logger)
	if err != nil {
		return fmt.Errorf("prepare web console: %w", err)
	}
	if !console.Built() {
		logger.WarnContext(ctx, "no web console embedded; the API is unaffected",
			"hint", "run `make build`")
	}

	handler := api.NewHandler(api.Options{
		Logger:   logger,
		GinMode:  ginMode(cfg.LogLevel),
		System:   systemService,
		Auth:     authService,
		KeyPairs: keyPairs,
		Signer:   signer,
		ReadyChecks: []api.Check{
			{Name: "db", Probe: db.Ping},
			{Name: "hypervisor", Probe: hv.Ping},
		},
		RefreshTTL:     cfg.RefreshTTL,
		CookieSecure:   cfg.CookieSecure,
		AllowedOrigins: cfg.AllowedOrigins,
		Console:        console,
	})

	server := api.NewServer(logger, cfg.Listen, handler.Router())

	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.InfoContext(context.WithoutCancel(ctx), "kumod stopped")

	return nil
}

// newHypervisor builds the backend selected by KUMO_HYPERVISOR. The libvirt
// implementation arrives in M3.1; until then kumod refuses to start with it
// instead of pretending instances could run.
func newHypervisor(cfg *config.Config) (hypervisor.Hypervisor, error) {
	switch cfg.Hypervisor {
	case config.HypervisorFake:
		return fake.New(fake.Config{Subnet: fakeSubnet}), nil
	case config.HypervisorLibvirt:
		return nil, fmt.Errorf("%w: the libvirt backend is not implemented yet (M3.1); "+
			"run with KUMO_HYPERVISOR=fake", hypervisor.ErrNotImplemented)
	default:
		return nil, fmt.Errorf("%w: %q", config.ErrInvalidHypervisor, cfg.Hypervisor)
	}
}

// ginMode keeps gin's own logs in step with KUMO_LOG_LEVEL.
func ginMode(level slog.Level) string {
	if level <= slog.LevelDebug {
		return gin.DebugMode
	}
	return gin.ReleaseMode
}
