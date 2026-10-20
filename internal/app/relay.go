package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

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
	flushTelemetry, err := setupTelemetry(ctx, opts.Version, logger)
	if err != nil {
		return err
	}
	defer flushTelemetry()

	// Check the broker first to expose invalid sink configuration, including missing newSink branches.
	sink, closeSink, err := newSink(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeSink()

	pool, err := connectPostgres(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	logger.Info("starting outbox relay", "sink", cfg.Events.Sink, "version", opts.Version)
	relay := &outbox.Relay{
		Source:       postgres.NewOutboxSource(pool),
		Sink:         sink,
		Logger:       logger,
		BatchSize:    cfg.Events.BatchSize,
		PollInterval: cfg.Events.PollInterval,
		Retention:    cfg.Events.Retention,
	}
	stopHealth, err := serveRelayHealth(ctx, cfg, opts, relay, logger)
	if err != nil {
		return err
	}
	defer stopHealth()

	err = relay.Run(ctx)
	logger.Info("outbox relay stopped")
	return err
}

// serveRelayHealth exposes GET /healthz: 200 while the relay loop progresses,
// 503 when stalled beyond relay.HealthThreshold() (Kubernetes restarts the pod).
// The relay has no readiness probe because it receives no traffic.
func serveRelayHealth(ctx context.Context, cfg config.Config, opts Options, relay *outbox.Relay, logger *slog.Logger) (stop func(), err error) {
	ln := opts.Listener
	if ln == nil {
		if ln, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Events.RelayHealthAddr); err != nil {
			return nil, fmt.Errorf("relay health listen: %w", err)
		}
	}
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
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("relay health server stopped", "error", err)
		}
	}()
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}, nil
}

// newSink selects the broker adapter and checks connectivity immediately to expose startup
// configuration errors. Each EventsSink value needs its own branch.
func newSink(ctx context.Context, cfg config.Config) (outbox.Sink, func(), error) {
	switch cfg.Events.Sink {
	case config.EventsSinkKafka:
		ev := cfg.Events
		opts := kafka.Options{SASLMechanism: ev.KafkaSASLMechanism, SASLUsername: ev.KafkaSASLUsername, SASLPassword: ev.KafkaSASLPassword}
		if ev.KafkaTLS {
			tlsCfg, err := kafka.TLSConfig(ev.KafkaTLSCAFile)
			if err != nil {
				return nil, nil, err
			}
			opts.TLS = tlsCfg
		}
		sink, err := kafka.NewSink(ev.KafkaBrokers, ev.KafkaTopic, opts)
		if err != nil {
			return nil, nil, err
		}
		pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := sink.Ping(pingCtx); err != nil {
			sink.Close()
			return nil, nil, err
		}
		return sink, sink.Close, nil
	default:
		return nil, nil, fmt.Errorf("unsupported events sink %q", cfg.Events.Sink)
	}
}
