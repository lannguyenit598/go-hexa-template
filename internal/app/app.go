// Package app là phần "bootstrap" của composition root: khởi tạo adapter, lắp chúng vào core
// và quản lý vòng đời HTTP server (readiness, drain khi tắt, timeout). Tách khỏi cmd/userapi để
// vòng đời này có test tự động; main chỉ còn đọc config, bắt tín hiệu và gọi Run.
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

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/config"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
	"github.com/example/userapi/internal/observability"
)

// Options cho Run. Giá trị zero dùng được cho production.
type Options struct {
	Version  string       // ghi vào log khởi động và service.version của OTel
	Listener net.Listener // nil = tự listen trên cfg.HTTPAddr; test truyền listener 127.0.0.1:0
}

// Run khởi tạo adapter và phục vụ HTTP cho tới khi ctx bị huỷ. Sau đó: /readyz trả 503,
// vẫn phục vụ trong cfg.ShutdownDelay, rồi ngừng nhận kết nối và chờ request đang chạy
// tối đa cfg.ShutdownTimeout. Trả nil khi tắt êm; trả lỗi khi khởi động thất bại hoặc drain quá hạn.
//
// Vì sao phải chờ ShutdownDelay: trên Kubernetes, việc gỡ pod khỏi Endpoints và cập nhật
// kube-proxy / ingress / LB diễn ra BẤT ĐỒNG BỘ với SIGTERM. Nếu đóng listener ngay, các request
// được định tuyến tới trong vài giây đó sẽ nhận 502 / connection refused ở mỗi lần deploy.
// Image distroless không có lệnh `sleep` cho preStop kiểu exec. Kubernetes 1.30+ có action
// preStop.sleep dựng sẵn, nhưng chờ trong app thì chạy được trên mọi nền tảng (ECS, Nomad...).
func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error {
	shutdownTelemetry, err := observability.Setup(ctx, opts.Version)
	if err != nil {
		return err
	}
	defer func() {
		// Flush span/metric còn trong bộ đệm; dùng ctx mới vì ctx đã bị huỷ.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTelemetry(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}()

	// ---- Outbound adapters ----
	repo, checkDB, closeRepo, err := newRepository(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer closeRepo()

	// ---- Core ----
	users := service.NewUserService(repo, system.NewUUIDv7(), system.Clock{}, logger)

	// ---- Inbound adapter ----
	var draining atomic.Bool
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          readiness(&draining, checkDB),
		RequestTimeout: cfg.RequestTimeout,
		TrustedProxies: cfg.TrustedProxies,
	})
	srv := &http.Server{
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

	// Listen đồng bộ để lỗi như "cổng đã bị chiếm" được trả về ngay từ Run.
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

	// Ngừng nhận kết nối mới, chờ request đang chạy xong (tối đa ShutdownTimeout).
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("userapi stopped gracefully")
	return nil
}

// Migrate áp dụng migration rồi trả về (lệnh `userapi migrate`).
func Migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if cfg.DB.URL == "" {
		return errors.New("DATABASE_URL is required for migrate")
	}
	return postgres.Migrate(ctx, cfg.DB.URL, logger)
}

// newRepository chọn adapter lưu trữ theo cấu hình. checkDB là nil khi readiness không ping DB.
func newRepository(ctx context.Context, cfg config.Config, logger *slog.Logger) (repo port.UserRepository, checkDB func(context.Context) error, closeFn func(), err error) {
	if cfg.Storage != config.StoragePostgres {
		return memory.NewUserRepository(), nil, func() {}, nil
	}

	pool, err := postgres.Connect(ctx, cfg.DB.URL, postgres.PoolOptions{
		MaxConns:         cfg.DB.MaxConns,
		MinConns:         cfg.DB.MinConns,
		MaxConnLifetime:  cfg.DB.MaxConnLifetime,
		MaxConnIdleTime:  cfg.DB.MaxConnIdleTime,
		StatementTimeout: cfg.DB.StatementTimeout,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(err error) (port.UserRepository, func(context.Context) error, func(), error) {
		pool.Close()
		return nil, nil, nil, err
	}
	if cfg.DB.MigrateOnStart {
		if err := postgres.Migrate(ctx, cfg.DB.URL, logger); err != nil {
			return fail(err)
		}
	}
	// Dừng sớm, rõ ràng, nếu schema cũ hơn code (Job migration chưa chạy / thất bại).
	pending, err := postgres.PendingMigrations(ctx, pool)
	if err != nil {
		return fail(err)
	}
	if pending {
		return fail(errors.New("database has pending migrations: run `userapi migrate` first (or set MIGRATE_ON_START=true)"))
	}
	if cfg.ReadinessCheckDB {
		checkDB = pool.Ping
	}
	return postgres.NewUserRepository(pool), checkDB, pool.Close, nil
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
