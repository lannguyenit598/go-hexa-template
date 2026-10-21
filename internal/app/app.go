// Package app bootstraps the composition root: build the Uber Fx graph that wires adapters into
// core, and manage the HTTP server lifecycle (readiness, shutdown drain, timeouts). Separate from
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
	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

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

// fatal carries the first runtime failure of a background component (HTTP server, relay loop) to run.
type fatal chan error

// report forwards err without blocking; the first failure wins.
func (f fatal) report(err error) {
	select {
	case f <- err:
	default:
	}
}

// dbCheck pings the database for readiness; nil when readiness does not ping DB.
type dbCheck func(context.Context) error

// runContext is the context passed to Run/RunRelay (canceled on SIGTERM). fx gets an uncancelable
// start context (see run), so start hooks that may wait long use abortOnRun to still honor it.
type runContext struct{ context.Context }

// abortOnRun returns hookCtx, additionally canceled when the run context is.
func abortOnRun(hookCtx context.Context, run runContext) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(hookCtx)
	stop := context.AfterFunc(run, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// Run creates adapters and serves HTTP until ctx is canceled. Then /readyz returns 503,
// requests are still served during cfg.ShutdownDelay, and the server stops accepting connections
// and drains for cfg.ShutdownTimeout. Returns nil on clean shutdown, or a startup/runtime/drain error.
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	return run(ctx, cfg, logger, opts, apiGraph(cfg))
}

// Migrate applies migrations and returns (`userapi migrate`).
func Migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.DB.URL == "" {
		return errors.New("DATABASE_URL is required for migrate")
	}
	return postgres.Migrate(ctx, cfg.DB.URL, logger)
}

// run starts the graph, waits for ctx or a runtime failure, and stops the graph.
// fx runs OnStart hooks in registration order and OnStop hooks in reverse.
func run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options, graph fx.Option) error {
	failed := make(fatal, 1)
	app := fx.New(appOptions(ctx, cfg, logger, opts, failed, graph))
	// Start without cancellation: fx rolls back a failed start with the start context, and a canceled
	// one skips every OnStop, leaking what already started. Hooks that wait on the database or broker
	// abort on ctx through abortOnRun instead, so a signal during startup still stops it promptly.
	if err := app.Start(context.WithoutCancel(ctx)); err != nil {
		return err
	}
	var runErr error
	select {
	case runErr = <-failed:
	case <-ctx.Done():
	}
	// No overall stop deadline: fx skips the remaining OnStop hooks once the stop context expires,
	// which would leave the pool open and telemetry unflushed. Each hook bounds itself instead
	// (ShutdownDelay + ShutdownTimeout for the drain, 5s telemetry flush, 2s relay health).
	return errors.Join(runErr, app.Stop(context.Background()))
}

// appOptions is the graph shared by every long-running command.
func appOptions(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options, failed fatal, graph fx.Option) fx.Option {
	return fx.Options(
		fx.Supply(cfg, logger, opts, failed, runContext{ctx}),
		fx.WithLogger(func() fxevent.Logger {
			l := &fxevent.SlogLogger{Logger: logger}
			// Graph events are noise at info. Start/Stop return every error (rollback included)
			// and main logs it once, so fx's own error events would only duplicate it.
			l.UseLogLevel(slog.LevelDebug)
			l.UseErrorLevel(slog.LevelDebug)
			return l
		}),
		// First hook, so telemetry flushes after everything else stops. It must be a module: fx runs
		// child modules' invokes before the parent's, so a root-level invoke would register last.
		fx.Module("telemetry", fx.Invoke(startTelemetry)),
		graph,
	)
}

// apiGraph wires the HTTP API: storage → service → server. The server depends on storage,
// so the pool starts before the server and closes after it drains.
func apiGraph(cfg config.Config) fx.Option {
	return fx.Options(
		storageModule(cfg),
		fx.Module("http",
			fx.Provide(newUserService, newHTTPServer),
			fx.Invoke(func(*http.Server) {}),
		),
	)
}

// storageModule selects the configured storage adapter. Every Storage value needs a branch:
// adding a constant without wiring it must fail startup.
func storageModule(cfg config.Config) fx.Option {
	switch cfg.Storage {
	case config.StorageMemory:
		return fx.Module("storage", fx.Provide(
			fx.Annotate(memory.NewUserRepository, fx.As(new(port.UserRepository))),
			func() dbCheck { return nil },
		))
	case config.StoragePostgres:
		return fx.Module("storage", fx.Provide(
			newPool,
			func(pool *pgxpool.Pool) port.UserRepository {
				// With an events sink, the relay (`userapi relay`) forwards outbox events to the broker.
				return postgres.NewUserRepository(pool, cfg.Events.Sink != config.EventsSinkNone)
			},
			func(pool *pgxpool.Pool) dbCheck {
				if cfg.ReadinessCheckDB {
					return pool.Ping
				}
				return nil
			},
		))
	default:
		return fx.Error(fmt.Errorf("unsupported storage %q", cfg.Storage))
	}
}

// newPool creates the shared pool without I/O. On start it pings, migrates if MIGRATE_ON_START is
// enabled, and fails early if the schema is older than the code (migration Job did not run or failed).
func newPool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger, run runContext) (*pgxpool.Pool, error) {
	return openPool(lc, cfg, logger, run, 0)
}

// openPool is newPool with an optional default idle_in_transaction_session_timeout (0 = none).
func openPool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger, run runContext, idleInTx time.Duration) (*pgxpool.Pool, error) {
	pool, err := postgres.NewPool(context.Background(), cfg.DB.URL, postgres.PoolOptions{
		MaxConns:                        cfg.DB.MaxConns,
		MinConns:                        cfg.DB.MinConns,
		MaxConnLifetime:                 cfg.DB.MaxConnLifetime,
		MaxConnIdleTime:                 cfg.DB.MaxConnIdleTime,
		StatementTimeout:                cfg.DB.StatementTimeout,
		DefaultIdleInTransactionTimeout: idleInTx,
	})
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := abortOnRun(ctx, run) // Ping and goose lock retries may wait long.
			defer cancel()
			// fx does not run OnStop for a hook whose OnStart failed: close here.
			if err := preparePostgres(ctx, cfg, pool, logger); err != nil {
				pool.Close()
				return err
			}
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
	return pool, nil
}

func preparePostgres(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, logger *slog.Logger) error {
	if err := postgres.Ping(ctx, pool); err != nil {
		return err
	}
	if cfg.DB.MigrateOnStart {
		if err := postgres.Migrate(ctx, cfg.DB.URL, logger); err != nil {
			return err
		}
	}
	pending, err := postgres.PendingMigrations(ctx, pool)
	if err != nil {
		return err
	}
	if pending {
		return errors.New("database has pending migrations: run `userapi migrate` first (or set MIGRATE_ON_START=true)")
	}
	return nil
}

func newUserService(repo port.UserRepository, logger *slog.Logger) port.UserService {
	return service.NewUserService(repo, system.UUIDv7{}, system.Clock{}, logger)
}

// newHTTPServer builds the server. On start it listens (synchronously, so errors such as an
// occupied port fail startup) and serves in the background; a serve failure stops the app.
//
// On stop it drains. ShutdownDelay is needed because Kubernetes removes pods from Endpoints and
// updates kube-proxy / ingress / load balancers ASYNCHRONOUSLY with SIGTERM. Closing the listener
// immediately causes 502 / connection refused for requests still routed there during deployment.
// Distroless has no sleep command for exec preStop. Kubernetes 1.30+ supports preStop.sleep,
// but waiting inside the app works on other platforms too (ECS, Nomad...).
func newHTTPServer(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger, opts Options,
	users port.UserService, checkDB dbCheck, failed fatal,
) *http.Server {
	var draining atomic.Bool
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          readiness(&draining, checkDB),
		RequestTimeout: cfg.RequestTimeout,
		TrustedProxies: cfg.TrustedProxies,
		APIDocs:        cfg.APIDocsEnabled,
	})
	// Root request context: cancel when draining times out so handlers stop and release database
	// connections before pool.Close(), which otherwise waits for running queries.
	baseCtx, cancelRequests := context.WithCancel(context.Background())
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
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln := opts.Listener
			if ln == nil {
				var err error
				if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTPAddr); err != nil {
					return fmt.Errorf("http listen: %w", err)
				}
			}
			go func() {
				if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
					failed.report(fmt.Errorf("http server: %w", err))
				}
			}()
			logger.Info("starting userapi", "addr", ln.Addr().String(), "storage", cfg.Storage, "version", opts.Version,
				"otel", observability.Enabled(os.Getenv))
			return nil
		},
		OnStop: func(context.Context) error {
			defer cancelRequests()
			draining.Store(true)
			logger.Info("shutting down, draining", "delay", cfg.ShutdownDelay.String())
			time.Sleep(cfg.ShutdownDelay)

			// Stop accepting connections and wait for active requests (up to ShutdownTimeout).
			shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
			defer cancel()
			if err := srv.Shutdown(shutdownCtx); err != nil {
				// On timeout, cancel remaining requests and close connections before the pool closes.
				// Still return an error (nonzero exit code) to expose the failed drain.
				cancelRequests()
				_ = srv.Close()
				return fmt.Errorf("graceful shutdown: %w", err)
			}
			logger.Info("userapi stopped gracefully")
			return nil
		},
	})
	return srv
}

// startTelemetry enables OpenTelemetry on start and flushes buffered spans/metrics on stop.
// Instruments created earlier by constructors (otelpgx, otelhttp) come from the global providers,
// which delegate to the SDK once Setup installs it.
func startTelemetry(lc fx.Lifecycle, opts Options, logger *slog.Logger) {
	var shutdown observability.Shutdown
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) (err error) {
			shutdown, err = observability.Setup(ctx, opts.Version)
			return err
		},
		OnStop: func(context.Context) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(ctx); err != nil {
				logger.Warn("telemetry shutdown failed", "error", err)
			}
			return nil
		},
	})
}

var errDraining = errors.New("shutting down")

// readiness reflects this pod's own state. Database ping is optional
// (READINESS_CHECK_DB) with a short timeout because probes default to 1s.
func readiness(draining *atomic.Bool, checkDB dbCheck) func(context.Context) error {
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
