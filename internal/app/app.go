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
	users := service.NewUserService(repo, system.NewUUIDv7(), system.Clock{}, logger)

	// ---- Inbound adapter ----
	var draining atomic.Bool
	e := httpapi.NewServer(users, httpapi.Options{
		Logger:         logger,
		Ready:          readiness(&draining, checkDB),
		RequestTimeout: cfg.RequestTimeout,
		TrustedProxies: cfg.TrustedProxies,
	})
	// Context gốc của mọi request: bị huỷ khi drain quá hạn, để handler còn chạy dừng lại và trả
	// kết nối database trước khi pool đóng (nếu không, pool.Close() chờ query chạy xong).
	baseCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	srv := &http.Server{
		Handler:     httpapi.Instrument(e),
		BaseContext: func(net.Listener) context.Context { return baseCtx },
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
		// Quá hạn: huỷ request còn lại và đóng hẳn các kết nối. Vẫn trả lỗi (exit code khác 0)
		// để người vận hành thấy drain không kịp, nhưng tiến trình thoát ngay sau đó.
		cancelRequests()
		_ = srv.Close()
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
// Mỗi giá trị Storage phải có nhánh riêng: thêm hằng mới mà quên nhánh thì app từ chối khởi động.
func newRepository(ctx context.Context, cfg config.Config, logger *slog.Logger) (repo port.UserRepository, checkDB func(context.Context) error, closeFn func(), err error) {
	switch cfg.Storage {
	case config.StorageMemory:
		return memory.NewUserRepository(), nil, func() {}, nil
	case config.StoragePostgres:
		return newPostgresRepository(ctx, cfg, logger)
	default:
		return nil, nil, nil, fmt.Errorf("unsupported storage %q", cfg.Storage)
	}
}

func newPostgresRepository(ctx context.Context, cfg config.Config, logger *slog.Logger) (port.UserRepository, func(context.Context) error, func(), error) {
	pool, err := connectPostgres(ctx, cfg, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	var checkDB func(context.Context) error
	if cfg.ReadinessCheckDB {
		checkDB = pool.Ping
	}
	var opts []postgres.Option
	if cfg.Events.Sink != config.EventsSinkNone {
		opts = append(opts, postgres.WithOutbox()) // relay (`userapi relay`) đẩy outbox lên broker
	}
	return postgres.NewUserRepository(pool, opts...), checkDB, pool.Close, nil
}

// connectPostgres mở pool, chạy migration nếu MIGRATE_ON_START, và dừng sớm, rõ ràng, nếu schema
// cũ hơn code (Job migration chưa chạy / thất bại).
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

// setupTelemetry bật OpenTelemetry; hàm trả về flush span/metric còn trong bộ đệm khi tắt.
func setupTelemetry(ctx context.Context, version string, logger *slog.Logger) (flush func(), err error) {
	shutdown, err := observability.Setup(ctx, version)
	if err != nil {
		return nil, err
	}
	return func() {
		// ctx mới vì ctx của tiến trình đã bị huỷ lúc này.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			logger.Warn("telemetry shutdown failed", "error", err)
		}
	}, nil
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
