package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	objectfake "github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/rustfs"
	"github.com/ahmedmohamedut/kumo/internal/service/accesskey"
	authservice "github.com/ahmedmohamedut/kumo/internal/service/auth"
	"github.com/ahmedmohamedut/kumo/internal/service/bucket"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instance"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/service/vpc"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/version"
	"github.com/ahmedmohamedut/kumo/internal/webui"
	"github.com/ahmedmohamedut/kumo/internal/worker"
	"github.com/ahmedmohamedut/kumo/internal/worker/locks"
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

	// Object storage is an optional module (docs/design/06-object-storage.md).
	// A server that is down at boot is not fatal: compute does not depend on
	// it, and /system/info reports it until it answers.
	objects, err := newObjectStore(cfg)
	if err != nil {
		return fmt.Errorf("build the object store client: %w", err)
	}
	if objects != nil {
		systemService.SetObjectStore(cfg.ObjectStore, objects)
		if err := objects.Ping(ctx); err != nil {
			logger.WarnContext(ctx, "the object store is not reachable; the Storage pages will say so",
				"backend", cfg.ObjectStore, "endpoint", cfg.S3Endpoint, "error", err)
		} else {
			logger.InfoContext(ctx, "object storage is ready",
				"backend", cfg.ObjectStore, "endpoint", cfg.S3Endpoint, "public_url", cfg.S3PublicURL)
		}
		if len(cfg.S3CORSOrigins) == 0 && cfg.ObjectStore == config.ObjectStoreRustFS {
			logger.WarnContext(ctx, "KUMO_S3_CORS_ORIGINS is empty: console uploads will be refused by the browser",
				"hint", "set it to the address you open the console at, e.g. https://kumo.example:8080")
		}
	}

	// The kumo.* presets are seeded once by migration 0003; from then on an
	// administrator manages the set through the API.
	instanceTypes, err := instancetype.New(instancetype.Options{Logger: logger, Store: db})
	if err != nil {
		return fmt.Errorf("build the instance type service: %w", err)
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

	// Subnets and the potentially conflicting operations on them share one lock
	// set: a launch allocates out of a subnet while a delete would remove the
	// row, and both have to take the very same key (subnet.LockKey).
	subnetLocks := locks.New()

	subnets, err := subnet.New(subnet.Options{Logger: logger, Store: db, Locks: subnetLocks})
	if err != nil {
		return fmt.Errorf("build the subnet service: %w", err)
	}

	// The vpc service owns the default VPC's bootstrap and the task handlers
	// that define, start, destroy and undefine a VPC's libvirt network.
	vpcs, err := vpc.New(vpc.Options{
		Logger:         logger,
		Store:          db,
		Hypervisor:     hv,
		Tasks:          tasks,
		Subnets:        subnets,
		DefaultVPCCIDR: cfg.DefaultVPCCIDR,
	})
	if err != nil {
		return fmt.Errorf("build the VPC service: %w", err)
	}

	// Every workflow attaches a guest to some network, so the default VPC (and
	// its default subnet) has to exist before the worker starts. Bootstrap is
	// deliberately not a task: it runs in-band, because the worker that would
	// run it is not started yet (docs/design/00-architecture.md §7, step 3).
	// A range the LAN already uses is not fatal: the node starts without a
	// default VPC and the operator creates one in the console.
	instanceNetwork := cfg.Network
	defaultVPC, err := vpcs.Bootstrap(ctx, cfg.Network)
	switch {
	case errors.Is(err, vpc.ErrDefaultSkipped):
		logger.WarnContext(ctx, "no default VPC: create one in the console before launching instances",
			"reason", err.Error())
	case err != nil:
		return fmt.Errorf("bootstrap the default VPC: %w", err)
	default:
		instanceNetwork = defaultVPC.LibvirtNetwork
		logger.InfoContext(ctx, "default VPC is ready",
			"vpc_id", defaultVPC.ID, "network", defaultVPC.LibvirtNetwork, "bridge", defaultVPC.BridgeName)
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

	// The databases module needs no server Kumo does not ship, so it is on
	// unless an operator turns it off. What it does need is an engine image,
	// which /system/info reports as an empty engine list rather than a fault
	// (docs/design/07-databases.md section 2). The adapter keeps the system
	// service from depending on the image service for one struct.
	systemService.SetDatabases(cfg.Databases, func(ctx context.Context) ([]system.DatabaseEngineInfo, error) {
		available, err := images.AvailableEngines(ctx)
		if err != nil {
			return nil, err
		}
		engines := make([]system.DatabaseEngineInfo, 0, len(available))
		for _, engine := range available {
			engines = append(engines, system.DatabaseEngineInfo{
				Engine:  string(engine.Engine),
				Version: engine.Version,
			})
		}
		return engines, nil
	})
	if cfg.Databases {
		if engines, err := images.AvailableEngines(ctx); err != nil {
			logger.WarnContext(ctx, "cannot list the database engines", "error", err)
		} else if len(engines) == 0 {
			logger.InfoContext(ctx, "databases are on but no engine image is available; "+
				"import one to create a DB instance",
				"hint", "packaging/images/postgres-18/README.md")
		} else {
			logger.InfoContext(ctx, "databases are ready", "engines", len(engines))
		}
	}

	// The launch workflow needs the pool layout to write a domain XML, which is
	// knowledge the backend has and the service does not. A backend that cannot
	// resolve a path is a backend that cannot boot a guest, so the wiring fails
	// rather than letting every launch fail later.
	resolver, ok := hv.(instance.VolumeResolver)
	if !ok {
		return fmt.Errorf("%w: the %s backend cannot resolve volume paths",
			config.ErrInvalidHypervisor, cfg.Hypervisor)
	}

	instances, err := instance.New(instance.Options{
		Logger:        logger,
		Store:         db,
		Hypervisor:    hv,
		Volumes:       resolver,
		Tasks:         tasks,
		Images:        images,
		Types:         instanceTypes,
		KeyPairs:      keyPairs,
		Capacity:      systemService,
		InstancesPool: cfg.PoolInstances,
		ImagesPool:    cfg.PoolImages,
		ConsoleDir:    consoleDir(cfg.DataDir),
		Network:       instanceNetwork,
		SubnetLocks:   subnetLocks,
		StopTimeout:   cfg.StopTimeout,
		Limits: capacity.Limits{
			MemoryReserveMiB: cfg.MemoryReserveMiB,
			VCPUOvercommit:   cfg.VCPUOvercommit,
		},
	})
	if err != nil {
		return fmt.Errorf("build the instance service: %w", err)
	}

	// Object storage is wired only when the module is on; a nil service makes
	// every /s3 route answer 503.
	var buckets *bucket.Service
	if objects != nil {
		buckets, err = bucket.New(bucket.Options{
			Logger:      logger,
			Store:       db,
			Objects:     objects,
			Tasks:       tasks,
			PublicURL:   cfg.S3PublicURL,
			Region:      cfg.S3Region,
			CORSOrigins: cfg.S3CORSOrigins,
			PresignTTL:  cfg.S3PresignTTL,
		})
		if err != nil {
			return fmt.Errorf("build the bucket service: %w", err)
		}
	}
	var accessKeys *accesskey.Service
	if objects != nil {
		accessKeys, err = accesskey.New(accesskey.Options{Logger: logger, Store: db, Objects: objects})
		if err != nil {
			return fmt.Errorf("build the access key service: %w", err)
		}
	}
	handlers := []worker.Registry{images.Handler(), instances.Handler(), vpcs.Handler()}
	if buckets != nil {
		handlers = append(handlers, buckets.Handler())
	}

	// The worker drains the tasks table. Recover runs before the first claim, so
	// a task the last process was interrupted on is retried instead of stranded.
	taskPool := worker.New(worker.Options{
		Logger:   logger,
		Store:    db,
		Registry: mergeHandlers(handlers...),
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

	// The reconciler keeps the rows in step with libvirt: a guest an operator
	// destroys with virsh, an address a guest renews into, a domain Kumo has no
	// row for (docs/design/00-architecture.md §4). It starts after the worker so
	// a pass never races the recovery of a task that still owns an instance.
	reconciler, err := worker.NewReconciler(worker.ReconcilerOptions{
		Logger: logger,
		Pass: func(ctx context.Context) error {
			report, err := instances.Reconcile(ctx)
			if err != nil {
				return err
			}
			if report.Updated > 0 || report.Orphans > 0 {
				logger.InfoContext(ctx, "reconciled",
					"rows", report.Rows, "domains", report.Domains,
					"updated", report.Updated, "skipped", report.Skipped,
					"orphans", report.Orphans)
			}
			return nil
		},
		Hint:   instances.ReconcileInstance,
		Events: hv.Events,
	})
	if err != nil {
		return fmt.Errorf("build the reconciler: %w", err)
	}
	if err := reconciler.Start(ctx); err != nil {
		return fmt.Errorf("start the reconciler: %w", err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := reconciler.Stop(stopCtx); err != nil {
			logger.ErrorContext(context.WithoutCancel(ctx), "the reconciler did not stop cleanly",
				"error", err)
		}
	}()

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
		Instances:     instances,
		VPCs:          vpcs,
		Subnets:       subnets,
		Buckets:       buckets,
		AccessKeys:    accessKeys,
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

// consoleDir is where the serial console log of every instance is written. It is
// derived from the data directory rather than configured separately: the logs
// belong to the same host state as the pools, and a configuration knob nobody
// would ever change is a knob nobody would ever document.
func consoleDir(dataDir string) string { return filepath.Join(dataDir, "console") }

// mergeHandlers combines the task handlers of several workflows into one registry.
func mergeHandlers(registries ...worker.Registry) worker.Registry {
	merged := worker.Registry{}
	for _, registry := range registries {
		for taskType, handler := range registry {
			merged[taskType] = handler
		}
	}
	return merged
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

// newObjectStore builds the backend selected by KUMO_OBJECTSTORE, or returns
// nil when the module is off.
func newObjectStore(cfg *config.Config) (objectstore.ObjectStore, error) {
	switch cfg.ObjectStore {
	case config.ObjectStoreNone:
		return nil, nil
	case config.ObjectStoreFake:
		return objectfake.New(), nil
	case config.ObjectStoreRustFS:
		creds, err := rustfs.ReadCredentials(cfg.S3CredentialsFile)
		if err != nil {
			return nil, err
		}
		return rustfs.New(rustfs.Options{
			Endpoint:  cfg.S3Endpoint,
			PublicURL: cfg.S3PublicURL,
			Region:    cfg.S3Region,
			AccessKey: creds.AccessKey,
			SecretKey: creds.SecretKey,
		})
	default:
		return nil, fmt.Errorf("%w: %q", config.ErrInvalidObjectStore, cfg.ObjectStore)
	}
}

// ginMode keeps gin's own logs in step with KUMO_LOG_LEVEL.
func ginMode(level slog.Level) string {
	if level <= slog.LevelDebug {
		return gin.DebugMode
	}
	return gin.ReleaseMode
}
