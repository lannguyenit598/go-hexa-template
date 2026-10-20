// Package app bootstraps the composition root: create adapters, wire them into core,
// and manage the HTTP server lifecycle (readiness, shutdown drain, timeouts). Separate from
// cmd/userapi for lifecycle tests; main only reads config, handles signals, and calls Run.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
	"github.com/example/userapi/internal/observability"
)

// Options configures Run. The zero value is suitable for production use.
type Options struct {
	Version string // Included in startup logs and OTel service.version.
	// Listener: nil = listen on cfg.HTTPAddr (Run) or cfg.Events.RelayHealthAddr (RunRelay);
	// tests supply a 127.0.0.1:0 listener.
	Listener net.Listener
}

// Run creates adapters and serves HTTP until ctx is canceled. Then /readyz returns 503,
// requests are still served during cfg.ShutdownDelay, and the server stops accepting connections
// and drains for cfg.ShutdownTimeout. Returns nil on clean shutdown, or a startup/drain error.
//
// ShutdownDelay is needed because Kubernetes removes pods from Endpoints and updates
// kube-proxy / ingress / load balancers ASYNCHRONOUSLY with SIGTERM. Closing the listener
// immediately causes 502 / connection refused for requests still routed there during deployment.
// Distroless has no sleep command for exec preStop. Kubernetes 1.30+ supports preStop.sleep,
// but waiting inside the app works on other platforms too (ECS, Nomad...).
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	flushTelemetry, err := setupTelemetry(ctx, opts.Version, logger)
	if err != nil {
		return err
	}
	defer flushTelemetry()

	// ---- Outbound adapters ----
	repo, checkDB, closeRepo, err := newRepository(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer closeRepo()

	// ---- Core ----
	users := service.NewUserService(repo, system.UUIDv7{}, system.Clock{}, logger)

	// ---- Inbound adapter ----
	var draining atomic.Bool
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          readiness(&draining, checkDB),
		RequestTimeout: cfg.RequestTimeout,
		TrustedProxies: cfg.TrustedProxies,
	})
	// Root request context: cancel when draining times out so handlers stop and release database
	// connections before pool.Close(), which otherwise waits for running queries.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := &http.Server{
		Handler:     httpapi.Instrument(e),
		BaseContext: func(net.Listener) context.Context { return baseCtx },
		// Prevent Slowloris: clients must finish sending headers within 5s.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Exceed REQUEST_TIMEOUT so handlers can return timeout errors before the connection closes.
		WriteTimeout: cfg.RequestTimeout + 5*time.Second,
		// Must EXCEED the load balancer idle timeout (AWS ALB default 60s). Otherwise the load balancer
		// may reuse a keep-alive connection just closed by the server, causing intermittent 502s.
		IdleTimeout: cfg.HTTPIdleTimeout,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	// Listen synchronously so errors such as an occupied port return directly from Run.
	ln := opts.Listener
	if ln == nil {
		if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr); err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("starting userapi", "addr", ln.Addr().String(), "storage", cfg.Storage, "version", opts.Version,
		"otel", observability.Enabled(os.Getenv))

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	draining.Store(true)
	logger.Info("shutdown signal received, draining", "delay", cfg.ShutdownDelay.String())
	time.Sleep(cfg.ShutdownDelay)

	// Stop accepting connections and wait for active requests (up to ShutdownTimeout).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// On timeout, cancel remaining requests and close connections. Still return an error
		// (nonzero exit code) to expose the failed drain, but let the process exit immediately afterward.
		cancelRequests()
		_ = srv.Close()
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("userapi stopped gracefully")
	return nil
}

// Migrate applies migrations and returns (`userapi migrate`).
func Migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.DB.URL == "" {
		return errors.New("DATABASE_URL is required for migrate")
	}
	return postgres.Migrate(ctx, cfg.DB.URL, logger)
}

// newRepository selects the configured storage adapter. checkDB is nil when readiness does not ping DB.
// Every Storage value needs a branch: adding a constant without wiring it must fail startup.
func newRepository(ctx context.Context, cfg config.Config, logger *slog.Logger) (repo port.UserRepository, checkDB func(context.Context) error, closeFn func(), err error) {
	switch cfg.Storage {
	case config.StorageMemory:
		return memory.NewUserRepository(), nil, func() {}, nil
	case config.StoragePostgres:
		pool, err := connectPostgres(ctx, cfg, logger)
		if err != nil {
			return nil, nil, nil, err
		}
		if cfg.ReadinessCheckDB {
			checkDB = pool.Ping
		}
		// With an events sink, the relay (`userapi relay`) forwards outbox events to the broker.
		return postgres.NewUserRepository(pool, cfg.Events.Sink != config.EventsSinkNone), checkDB, pool.Close, nil
	default:
		return nil, nil, nil, fmt.Errorf("unsupported storage %q", cfg.Storage)
	}
}

// connectPostgres opens a pool, migrates if MIGRATE_ON_START is enabled, and fails early if the
// schema is older than the code (migration Job did not run or failed).
func connectPostgres(ctx context.Context, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := postgres.Connect(ctx, cfg.DB.URL, postgres.PoolOptions{
		MaxConns:         cfg.DB.MaxConns,
		MinConns:         cfg.DB.MinConns,
		MaxConnLifetime:  cfg.DB.MaxConnLifetime,
		MaxConnIdleTime:  cfg.DB.MaxConnIdleTime,
		StatementTimeout: cfg.DB.StatementTimeout,
	})
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*pgxpool.Pool, error) {
		pool.Close()
		return nil, err
	}
	if cfg.DB.MigrateOnStart {
		if err := postgres.Migrate(ctx, cfg.DB.URL, logger); err != nil {
			return fail(err)
		}
	}
	pending, err := postgres.PendingMigrations(ctx, pool)
	if err != nil {
		return fail(err)
	}
	if pending {
		return fail(errors.New("database has pending migrations: run `userapi migrate` first (or set MIGRATE_ON_START=true)"))
	}
	return pool, nil
}

// setupTelemetry enables OpenTelemetry and returns a function that flushes buffered spans/metrics on shutdown.
func setupTelemetry(ctx context.Context, version string, logger *slog.Logger) (flush func(), err error) {
	shutdown, err := observability.Setup(ctx, version)
	if err != nil {
		return nil, err
	}
	return func() {
		// Use a fresh ctx because the process context is already canceled.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}, nil
}

var errDraining = errors.New("shutting down")

// readiness reflects this pod's own state. Database ping is optional
// (READINESS_CHECK_DB) with a short timeout because probes default to 1s.
func readiness(draining *atomic.Bool, checkDB func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		if draining.Load() {
			return errDraining
		}
		if checkDB == nil {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer cancel()
		return checkDB(ctx)
	}
}
