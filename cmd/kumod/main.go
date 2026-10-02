// Command kumod is the Kumo control plane: HTTP API, task worker and the
// embedded web console. This binary is built by `make build`.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"

	"github.com/ahmedmohamedut/kumo/internal/api"
	"github.com/ahmedmohamedut/kumo/internal/config"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
	"github.com/ahmedmohamedut/kumo/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx); err != nil {
		slog.ErrorContext(context.WithoutCancel(ctx), "kumod exited with an error", "error", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logging.New(cfg.LogLevel, os.Stdout)
	slog.SetDefault(logger)

	logger.InfoContext(ctx, "starting kumod",
		"version", version.Version,
		"commit", version.Commit,
		"listen", cfg.Listen,
		"hypervisor", cfg.Hypervisor,
		"data_dir", cfg.DataDir,
	)

	// The store and the hypervisor are wired in M0.3 and M0.4; until then
	// /api/v1/system/info reports the configured hypervisor without probing it.
	systemService := system.New(logger, version.Version, cfg.Hypervisor, nil, nil)

	handler := api.NewHandler(api.Options{
		Logger:         logger,
		GinMode:        ginMode(cfg.LogLevel),
		System:         systemService,
		AllowedOrigins: cfg.AllowedOrigins,
	})

	server := api.NewServer(logger, cfg.Listen, handler.Router())

	if err := server.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	logger.InfoContext(context.WithoutCancel(ctx), "kumod stopped")
	return nil
}

// ginMode keeps gin's own logs in step with KUMO_LOG_LEVEL.
func ginMode(level slog.Level) string {
	if level <= slog.LevelDebug {
		return gin.DebugMode
	}
	return gin.ReleaseMode
}
