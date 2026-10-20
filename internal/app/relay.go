package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/example/userapi/internal/adapter/outbound/kafka"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/outbox"
)

// RunRelay chạy relay của transactional outbox (lệnh `userapi relay`): đọc sự kiện mà API đã ghi
// vào bảng outbox và gửi lên broker, cho tới khi ctx bị huỷ. Chạy thành tiến trình riêng (cùng
// image với API) để scale và khởi động lại độc lập với API.
func RunRelay(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	if cfg.Events.Sink == config.EventsSinkNone {
		return errors.New("EVENTS_SINK=none: there is nothing to relay (set EVENTS_SINK, see docs/events.md)")
	}
	flushTelemetry, err := setupTelemetry(ctx, opts.Version, logger)
	if err != nil {
		return err
	}
	defer flushTelemetry()

	pool, err := connectPostgres(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	sink, closeSink, err := newSink(ctx, cfg)
	if err != nil {
		return err
	}
	defer closeSink()

	logger.Info("starting outbox relay", "sink", cfg.Events.Sink, "version", opts.Version)
	relay := &outbox.Relay{
		Source:       postgres.NewOutboxSource(pool),
		Sink:         sink,
		Logger:       logger,
		BatchSize:    cfg.Events.BatchSize,
		PollInterval: cfg.Events.PollInterval,
		Retention:    cfg.Events.Retention,
	}
	err = relay.Run(ctx)
	logger.Info("outbox relay stopped")
	return err
}

// newSink chọn adapter broker theo cấu hình, và kiểm tra kết nối ngay để cấu hình sai lộ ra lúc
// khởi động. Mỗi giá trị EventsSink phải có nhánh riêng.
func newSink(ctx context.Context, cfg config.Config) (outbox.Sink, func(), error) {
	switch cfg.Events.Sink {
	case config.EventsSinkKafka:
		sink, err := kafka.NewSink(cfg.Events.KafkaBrokers, cfg.Events.KafkaTopic)
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
