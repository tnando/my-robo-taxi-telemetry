// Package server manages the HTTP listeners for the telemetry service.
// It binds three ports: Tesla (mTLS for vehicle connections), Client
// (WebSocket for browsers), and Metrics (Prometheus + health checks).
// All three are started and stopped together via Start/Shutdown.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 15 * time.Second
)

// Server manages the three HTTP listeners (Tesla, Client, Metrics) and
// their lifecycle. Create one with New, call Start to begin serving, and
// Shutdown to stop gracefully.
type Server struct {
	tesla   *http.Server
	client  *http.Server
	metrics *http.Server
	logger  *slog.Logger
}

// New creates a Server with three HTTP servers configured on the ports
// specified in cfg. The metrics server exposes /healthz, /readyz, and
// /metrics. Tesla and client servers return 404 until their handlers are
// wired in future issues.
func New(cfg config.ServerConfig, logger *slog.Logger, checker ReadinessChecker, reg *prometheus.Registry) *Server {
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", handleHealthz)
	metricsMux.HandleFunc("GET /readyz", handleReadyz(checker, logger))
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	skipPaths := map[string]struct{}{
		"/healthz": {},
	}
	logMiddleware := requestLogger(logger, skipPaths)

	placeholder := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotFound)
	})

	return &Server{
		tesla: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.TeslaPort),
			Handler:           logMiddleware(placeholder),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		client: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.ClientPort),
			Handler:           logMiddleware(placeholder),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		metrics: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
			Handler:           logMiddleware(metricsMux),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		logger: logger,
	}
}

// Start begins serving on all three ports. It blocks until ctx is
// cancelled or one of the servers returns a fatal error. On context
// cancellation it initiates a graceful shutdown with a fixed timeout.
func (s *Server) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return s.serve(ctx, "tesla", s.tesla) })
	g.Go(func() error { return s.serve(ctx, "client", s.client) })
	g.Go(func() error { return s.serve(ctx, "metrics", s.metrics) })

	if err := g.Wait(); err != nil {
		return fmt.Errorf("server.Start: %w", err)
	}
	return nil
}

// Shutdown gracefully stops all three servers. It uses its own timeout
// context so that shutdown proceeds even if the caller's context is
// already cancelled.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// If the caller provided a tighter deadline, respect it.
	if deadline, ok := ctx.Deadline(); ok {
		if deadline.Before(time.Now().Add(shutdownTimeout)) {
			shutdownCtx = ctx
		}
	}

	var errs []error
	for _, pair := range []struct {
		name string
		srv  *http.Server
	}{
		{"tesla", s.tesla},
		{"client", s.client},
		{"metrics", s.metrics},
	} {
		if err := pair.srv.Shutdown(shutdownCtx); err != nil {
			errs = append(errs, fmt.Errorf("server.Shutdown(%s): %w", pair.name, err))
		} else {
			s.logger.Info("server stopped", slog.String("name", pair.name))
		}
	}

	return errors.Join(errs...)
}

// serve starts an HTTP server and shuts it down when ctx is cancelled.
func (s *Server) serve(ctx context.Context, name string, srv *http.Server) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("server.serve(%s): listen %s: %w", name, srv.Addr, err)
	}
	s.logger.Info("server listening",
		slog.String("name", name),
		slog.String("addr", ln.Addr().String()),
	)

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("server.serve(%s): %w", name, err)
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		// Context cancelled — initiate shutdown from the Start errgroup.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server.serve(%s): shutdown: %w", name, err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}
