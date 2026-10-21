package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/example/userapi/internal/adapter/outbound/kafka"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/outbox"
)

// RunRelay runs the transactional outbox relay (`userapi relay`): read API-written events
// from the outbox and send them to the broker until ctx is canceled. Run in a separate process
// using the API image, allowing independent scaling and restarts.
func RunRelay(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	if cfg.Events.Sink == config.EventsSinkNone {
		return errors.New("EVENTS_SINK=none: there is nothing to relay (set EVENTS_SINK, see docs/events.md)")
	}
	return run(ctx, cfg, logger, opts, relayGraph(cfg))
}

// relayIdleInTxTimeout exceeds the relay's 30s batch: the server ends a batch transaction whose
// relay vanished (releasing its outbox row locks) unless the deployment configured its own limit.
const relayIdleInTxTimeout = 60 * time.Second

// relayGraph wires the relay. Invoke order fixes hook order: the broker is checked before the
// database to expose invalid sink configuration (including missing sinkModule branches) first;
// on stop, the health server and relay loop stop before the pool and sink close.
func relayGraph(cfg config.Config) fx.Option {
	return fx.Options(
		sinkModule(cfg),
		fx.Module("relay",
			fx.Provide(newRelayPool, newRelay),
			fx.Invoke(
				func(outbox.Sink) {},
				func(*pgxpool.Pool) {},
				serveRelayHealth,
			),
		),
	)
}

// newRelayPool is newPool with relayIdleInTxTimeout as the idle-in-transaction default.
func newRelayPool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger, run runContext) (*pgxpool.Pool, error) {
	return openPool(lc, cfg, logger, run, relayIdleInTxTimeout)
}

// newRelay builds the relay and runs its loop from start until stop. The loop context is separate
// from the start context, which only bounds startup.
func newRelay(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger, opts Options,
	pool *pgxpool.Pool, sink outbox.Sink, failed fatal,
) *outbox.Relay {
	relay := &outbox.Relay{
		Source:       postgres.NewOutboxSource(pool),
		Sink:         sink,
		Logger:       logger,
		BatchSize:    cfg.Events.BatchSize,
		PollInterval: cfg.Events.PollInterval,
		Retention:    cfg.Events.Retention,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			logger.Info("starting outbox relay", "sink", cfg.Events.Sink, "version", opts.Version)
			go func() {
				defer close(done)
				if err := relay.Run(ctx); err != nil {
					failed.report(fmt.Errorf("outbox relay: %w", err))
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			cancel()
			<-done // Bounded by the relay's batch timeout.
			logger.Info("outbox relay stopped")
			return nil
		},
	})
	return relay
}

// serveRelayHealth exposes GET /healthz: 200 while the relay loop progresses,
// 503 when stalled beyond relay.HealthThreshold() (Kubernetes restarts the pod).
// The relay has no readiness probe because it receives no traffic.
func serveRelayHealth(lc fx.Lifecycle, cfg config.Config, opts Options, relay *outbox.Relay, logger *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if relay.Healthy(time.Now()) {
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"status":"stalled"}`))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ln := opts.Listener
			if ln == nil {
				var err error
				if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Events.RelayHealthAddr); err != nil {
					return fmt.Errorf("relay health listen: %w", err)
				}
			}
			go func() {
				if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("relay health server stopped", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(context.Context) error {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			return nil
		},
	})
}

// sinkModule selects the broker adapter. Each EventsSink value needs its own branch.
func sinkModule(cfg config.Config) fx.Option {
	switch cfg.Events.Sink {
	case config.EventsSinkKafka:
		return fx.Module("sink", fx.Provide(newKafkaSink))
	default:
		return fx.Error(fmt.Errorf("unsupported events sink %q", cfg.Events.Sink))
	}
}

// newKafkaSink creates the client without I/O and checks connectivity on start to expose
// startup configuration errors.
func newKafkaSink(lc fx.Lifecycle, cfg config.Config, run runContext) (outbox.Sink, error) {
	ev := cfg.Events
	opts := kafka.Options{SASLMechanism: ev.KafkaSASLMechanism, SASLUsername: ev.KafkaSASLUsername, SASLPassword: ev.KafkaSASLPassword}
	if ev.KafkaTLS {
		tlsCfg, err := kafka.TLSConfig(ev.KafkaTLSCAFile)
		if err != nil {
			return nil, err
		}
		opts.TLS = tlsCfg
	}
	sink, err := kafka.NewSink(ev.KafkaBrokers, ev.KafkaTopic, opts)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := abortOnRun(ctx, run)
			defer cancel()
			pingCtx, cancelPing := context.WithTimeout(ctx, 10*time.Second)
			defer cancelPing()
			if err := sink.Ping(pingCtx); err != nil {
				sink.Close() // fx does not run OnStop for a hook whose OnStart failed.
				return err
			}
			return nil
		},
		OnStop: func(context.Context) error {
			sink.Close()
			return nil
		},
	})
	return sink, nil
}
