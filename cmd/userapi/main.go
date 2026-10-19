// Command userapi là "composition root": nơi duy nhất biết TẤT CẢ các adapter
// và lắp chúng vào core. Đổi storage, đổi transport... chỉ sửa ở đây.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

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
	if err := run(); err != nil {
		slog.Error("userapi stopped with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	// Hủy ctx khi nhận SIGINT/SIGTERM => Echo bắt đầu graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ---- Outbound adapters ----
	var (
		repo  port.UserRepository
		ready func(context.Context) error
	)
	switch cfg.Storage {
	case config.StoragePostgres:
		pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer pool.Close()
		if err := postgres.Migrate(ctx, pool); err != nil {
			return err
		}
		repo = postgres.NewUserRepository(pool)
		ready = pool.Ping
	default:
		repo = memory.NewUserRepository()
	}

	// ---- Core ----
	users := service.NewUserService(repo, system.NewUUIDv7(), system.Clock{})

	// ---- Inbound adapter ----
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          ready,
		RequestTimeout: cfg.RequestTimeout,
	})

	logger.Info("starting userapi", "addr", cfg.HTTPAddr, "storage", cfg.Storage)
	sc := echo.StartConfig{
		Address:         cfg.HTTPAddr,
		HideBanner:      true,
		GracefulTimeout: cfg.ShutdownTimeout,
	}
	if err := sc.Start(ctx, e); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info("userapi stopped gracefully")
	return nil
}
