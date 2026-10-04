package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/auth"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	libvirt "github.com/ahmedmohamedut/kumo/internal/hypervisor/libvirt"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/version"
	"github.com/ahmedmohamedut/kumo/internal/webui"
	"github.com/ahmedmohamedut/kumo/internal/worker"
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

	systemService := system.New(logger, version.Version, cfg.Hypervisor, db, hv, db)
	systemService.SetLimits(capacity.Limits{
		MemoryReserveMiB: cfg.MemoryReserveMiB,
		VCPUOvercommit:   cfg.VCPUOvercommit,
	})

	// The instance type presets are data, not schema, so they are seeded on
	// every boot: INSERT OR IGNORE keeps a preset an operator edited by hand.
	instanceTypes, err := instancetype.New(instancetype.Options{Store: db})
	if err != nil {
		return fmt.Errorf("build the instance type service: %w", err)
	}
	if err := instanceTypes.Seed(ctx); err != nil {
		return fmt.Errorf("seed the instance types: %w", err)
	}

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

	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		return fmt.Errorf("build the task service: %w", err)
	}

	// The staging directory holds a download until it becomes a volume, so it
	// has to exist before the worker runs; putting it under the data directory
	// keeps it on the same filesystem as the pool.
	if err := os.MkdirAll(cfg.ImageTempDir, 0o700); err != nil {
		return fmt.Errorf("create the image staging directory %s: %w", cfg.ImageTempDir, err)
	}

	images, err := image.New(image.Options{
		Logger:     logger,
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Downloader: imagefetch.New(imagefetch.Options{
			MaxBytes:             cfg.ImageMaxBytes,
			AllowPrivateNetworks: cfg.ImageAllowPrivateURLs,
		}),
		ImagesPool: cfg.PoolImages,
		TempDir:    cfg.ImageTempDir,
		MaxBytes:   cfg.ImageMaxBytes,
	})
	if err != nil {
		return fmt.Errorf("build the image service: %w", err)
	}

	// The worker drains the tasks table. Recover runs before the first claim, so
	// a task the last process was interrupted on is retried instead of stranded.
	taskPool := worker.New(worker.Options{
		Logger:   logger,
		Store:    db,
		Registry: worker.Registry(images.Handler()),
		Workers:  cfg.Workers,
	})
	if err := taskPool.Start(ctx); err != nil {
		return fmt.Errorf("start the task worker: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := taskPool.Stop(stopCtx); err != nil {
			logger.ErrorContext(context.WithoutCancel(ctx), "the task worker did not stop cleanly",
				"error", err)
		}
	}()
	logger.InfoContext(ctx, "task worker is running", "workers", cfg.Workers)

	console, err := webui.New(logger)
	if err != nil {
		return fmt.Errorf("prepare web console: %w", err)
	}
	if !console.Built() {
		logger.WarnContext(ctx, "no web console embedded; the API is unaffected",
			"hint", "run `make build`")
	}

	handler := api.NewHandler(api.Options{
		Logger:        logger,
		GinMode:       ginMode(cfg.LogLevel),
		System:        systemService,
		Auth:          authService,
		KeyPairs:      keyPairs,
		Images:        images,
		InstanceTypes: instanceTypes,
		Tasks:         tasks,
		Signer:        signer,
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
		return libvirt.New(logging.New(cfg.LogLevel, os.Stdout), cfg.LibvirtURI)
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
