package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// Server timeouts. WriteTimeout stays unset on purpose: /events (SSE) and the
// console WebSocket will hold requests open.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultShutdownTimeout   = 15 * time.Second
)

// Server wraps http.Server with graceful shutdown on context cancellation.
type Server struct {
	Addr              string
	Handler           http.Handler
	Logger            *slog.Logger
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
}

// NewServer builds the HTTP server.
func NewServer(logger *slog.Logger, addr string, handler http.Handler) *Server {
	return &Server{
		Addr:              addr,
		Handler:           handler,
		Logger:            logger,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		IdleTimeout:       defaultIdleTimeout,
		ShutdownTimeout:   defaultShutdownTimeout,
	}
}

// Run listens on the configured address and serves until ctx is canceled.
func (s *Server) Run(ctx context.Context) error {
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.Addr, err)
	}
	return s.Serve(ctx, listener)
}

// Serve serves on ln until ctx is canceled, then drains in-flight requests
// within ShutdownTimeout.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	server := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: s.ReadHeaderTimeout,
		IdleTimeout:       s.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(s.Logger.Handler(), slog.LevelWarn),
	}

	served := make(chan error, 1)
	go func() {
		s.Logger.InfoContext(ctx, "http server listening", "address", ln.Addr().String())
		err := server.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		served <- err
	}()

	select {
	case err := <-served:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
	}

	s.Logger.InfoContext(context.WithoutCancel(ctx), "shutting down http server",
		"timeout", s.ShutdownTimeout)

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.ShutdownTimeout)
	defer cancel()

	shutdownErr := server.Shutdown(shutdownCtx)
	serveErr := <-served

	if shutdownErr != nil {
		return fmt.Errorf("shutdown: %w", shutdownErr)
	}
	if serveErr != nil {
		return fmt.Errorf("serve: %w", serveErr)
	}

	s.Logger.InfoContext(context.WithoutCancel(ctx), "http server stopped")
	return nil
}
