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

	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
)

func main() {
	cfg, err := config.FromEnv()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
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

	logger.Info("starting userapi", "addr", cfg.HTTPAddr, "storage", cfg.Storage)
	sc := echo.StartConfig{
		Address:         cfg.HTTPAddr,
		HideBanner:      true,
		GracefulTimeout: cfg.ShutdownTimeout,
	}
	if err := sc.Start(serverCtx, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("userapi stopped gracefully")
	return nil
}

// shutdownOnSignal: khi nhận tín hiệu, đánh dấu draining (/readyz trả 503), chờ ShutdownDelay
// rồi mới huỷ serverCtx để Echo ngừng nhận kết nối và drain request đang chạy.
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
