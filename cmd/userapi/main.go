// Command userapi là "composition root": nơi duy nhất biết TẤT CẢ các adapter
// và lắp chúng vào core. Đổi storage, đổi transport... chỉ sửa ở đây.
//
// Cách dùng:
//
//	userapi           chạy HTTP API
//	userapi migrate   áp dụng migration PostgreSQL rồi thoát (dùng cho Job / initContainer)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
	"github.com/example/userapi/internal/observability"
)

// version được gán lúc build: go build -ldflags "-X main.version=$(git describe --tags)".
var version = "dev"

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	// ContextHandler thêm request_id, trace_id, span_id từ ctx vào mọi dòng log *Context(ctx, ...).
	logger := slog.New(observability.NewContextHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}),
	))
	slog.SetDefault(logger)

	switch cmd := strings.Join(os.Args[1:], " "); cmd {
	case "":
		err = run(cfg, logger)
	case "migrate":
		err = migrate(cfg, logger)
	default:
		err = fmt.Errorf("unknown command %q (usage: userapi [migrate])", cmd)
	}
	if err != nil {
		logger.Error("userapi stopped with error", "error", err)
		os.Exit(1)
	}
}

func migrate(cfg config.Config, logger *slog.Logger) error {
	if cfg.DB.URL == "" {
		return errors.New("DATABASE_URL is required for migrate")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return postgres.Migrate(ctx, cfg.DB.URL, logger)
}

func run(cfg config.Config, logger *slog.Logger) error {
	// sigCtx bị huỷ khi nhận SIGINT/SIGTERM; serverCtx bị huỷ SAU ShutdownDelay (xem shutdownOnSignal).
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serverCtx, cancelServer := context.WithCancel(context.Background())
	defer cancelServer()

	var draining atomic.Bool
	go shutdownOnSignal(sigCtx, stop, cancelServer, &draining, cfg.ShutdownDelay, logger)

	shutdownTelemetry, err := observability.Setup(sigCtx, version)
	if err != nil {
		return err
	}
	defer func() {
		// Flush span/metric còn trong bộ đệm; dùng ctx mới vì sigCtx đã bị huỷ.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()

	// ---- Outbound adapters ----
	var (
		repo    port.UserRepository
		checkDB func(context.Context) error
	)
	switch cfg.Storage {
	case config.StoragePostgres:
		pool, err := postgres.Connect(sigCtx, cfg.DB.URL, postgres.PoolOptions{
			MaxConns:         cfg.DB.MaxConns,
			MinConns:         cfg.DB.MinConns,
			MaxConnLifetime:  cfg.DB.MaxConnLifetime,
			MaxConnIdleTime:  cfg.DB.MaxConnIdleTime,
			StatementTimeout: cfg.DB.StatementTimeout,
		})
		if err != nil {
			return err
		}
		defer pool.Close()
		if cfg.DB.MigrateOnStart {
			if err := postgres.Migrate(sigCtx, cfg.DB.URL, logger); err != nil {
				return err
			}
		}
		// Dừng sớm, rõ ràng, nếu schema cũ hơn code (Job migration chưa chạy / thất bại).
		pending, err := postgres.PendingMigrations(sigCtx, pool)
		if err != nil {
			return err
		}
		if pending {
			return errors.New("database has pending migrations: run `userapi migrate` first (or set MIGRATE_ON_START=true)")
		}
		repo = postgres.NewUserRepository(pool)
		if cfg.ReadinessCheckDB {
			checkDB = pool.Ping
		}
	default:
		repo = memory.NewUserRepository()
	}

	// ---- Core ----
	users := service.NewUserService(repo, system.NewUUIDv7(), system.Clock{})

	// ---- Inbound adapter ----
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          readiness(&draining, checkDB),
		RequestTimeout: cfg.RequestTimeout,
		TrustedProxies: cfg.TrustedProxies,
	})

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.Instrument(e),
		// Chặn Slowloris: client phải gửi xong header trong 5s.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Lớn hơn REQUEST_TIMEOUT để handler kịp trả lỗi timeout thay vì bị cắt kết nối giữa chừng.
		WriteTimeout: cfg.RequestTimeout + 5*time.Second,
		// Phải LỚN HƠN idle timeout của load balancer (AWS ALB mặc định 60s). Nếu nhỏ hơn, LB có
		// thể gửi request vào một keep-alive connection mà server vừa đóng => 502 lác đác.
		IdleTimeout: cfg.HTTPIdleTimeout,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	logger.Info("starting userapi", "addr", cfg.HTTPAddr, "storage", cfg.Storage, "version", version,
		"otel", observability.Enabled(os.Getenv))

	select {
	case err := <-serveErr:
		return fmt.Errorf("http server: %w", err) // ví dụ cổng đã bị chiếm
	case <-serverCtx.Done():
	}

	// Ngừng nhận kết nối mới, chờ request đang chạy xong (tối đa ShutdownTimeout).
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("userapi stopped gracefully")
	return nil
}

// shutdownOnSignal: khi nhận tín hiệu, đánh dấu draining (/readyz trả 503), chờ ShutdownDelay
// rồi mới huỷ serverCtx để server ngừng nhận kết nối và drain request đang chạy.
//
// Lý do phải chờ: trên Kubernetes, việc gỡ pod khỏi Endpoints và cập nhật kube-proxy /
// ingress / LB diễn ra BẤT ĐỒNG BỘ với SIGTERM. Nếu đóng listener ngay, các request được
// định tuyến tới trong vài giây đó sẽ nhận 502 / connection refused ở mỗi lần deploy.
// Image distroless không có lệnh sleep cho preStop, nên việc chờ được làm trong app.
func shutdownOnSignal(sigCtx context.Context, stopSignals func(), cancelServer func(), draining *atomic.Bool, delay time.Duration, logger *slog.Logger) {
	<-sigCtx.Done()
	draining.Store(true)
	// Bỏ đăng ký: tín hiệu thứ hai (Ctrl+C lần nữa) sẽ kết thúc tiến trình ngay.
	stopSignals()
	logger.Info("shutdown signal received, draining", "delay", delay.String())
	time.Sleep(delay)
	cancelServer()
}

var errDraining = errors.New("shutting down")

// readiness chỉ phản ánh trạng thái của chính pod. Ping database là tuỳ chọn
// (READINESS_CHECK_DB) và có timeout ngắn, vì probe mặc định timeout sau 1s.
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
