// Package config đọc cấu hình từ biến môi trường (12-factor).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Storage string

const (
	StorageMemory   Storage = "memory"
	StoragePostgres Storage = "postgres"
)

type Config struct {
	HTTPAddr string
	Storage  Storage
	LogLevel slog.Level

	// ShutdownDelay: thời gian chờ sau SIGTERM trước khi ngừng nhận kết nối, để
	// load balancer / kube-proxy kịp gỡ pod khỏi danh sách endpoint.
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration
	// HTTPIdleTimeout: thời gian giữ keep-alive connection rảnh. Phải lớn hơn idle timeout
	// của load balancer phía trước (AWS ALB: 60s), nếu không sẽ có 502 lác đác.
	HTTPIdleTimeout time.Duration

	// TrustedProxies: các dải CIDR của proxy tin cậy. Rỗng = lấy IP từ kết nối TCP.
	TrustedProxies []*net.IPNet

	// ReadinessCheckDB: /readyz có ping database hay không. Mặc định tắt để một sự cố
	// database ngắn không làm MỌI pod cùng lúc bị gỡ khỏi load balancer.
	ReadinessCheckDB bool

	DB DB
}

// DB là cấu hình pool kết nối PostgreSQL.
type DB struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	StatementTimeout time.Duration
	// MigrateOnStart: chạy migration khi app khởi động. Tiện cho dev; production nên chạy
	// `userapi migrate` như một Job / initContainer riêng (xem README).
	MigrateOnStart bool
}

// Load đọc env với lookup có thể thay thế (os.LookupEnv khi chạy thật, map khi test).
func Load(lookup func(string) (string, bool)) (Config, error) {
	p := parser{lookup: lookup}

	cfg := Config{
		HTTPAddr: p.str("HTTP_ADDR", ":8080"),
		Storage:  Storage(strings.ToLower(p.str("STORAGE", string(StorageMemory)))),
		DB: DB{
			URL:              p.str("DATABASE_URL", ""),
			MaxConns:         p.int32("DB_MAX_CONNS", 10),
			MinConns:         p.int32("DB_MIN_CONNS", 0),
			MaxConnLifetime:  p.duration("DB_MAX_CONN_LIFETIME", "30m"),
			MaxConnIdleTime:  p.duration("DB_MAX_CONN_IDLE_TIME", "5m"),
			StatementTimeout: p.duration("DB_STATEMENT_TIMEOUT", "5s"),
			MigrateOnStart:   p.bool("MIGRATE_ON_START", false),
		},
		ShutdownDelay:    p.duration("SHUTDOWN_DELAY", "0s"),
		ShutdownTimeout:  p.duration("SHUTDOWN_TIMEOUT", "10s"),
		RequestTimeout:   p.duration("REQUEST_TIMEOUT", "10s"),
		HTTPIdleTimeout:  p.duration("HTTP_IDLE_TIMEOUT", "75s"),
		ReadinessCheckDB: p.bool("READINESS_CHECK_DB", false),
		TrustedProxies:   p.cidrs("TRUSTED_PROXIES"),
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(p.str("LOG_LEVEL", "info"))); err != nil {
		p.fail("LOG_LEVEL: %w", err)
	}

	switch cfg.Storage {
	case StorageMemory:
	case StoragePostgres:
		if cfg.DB.URL == "" {
			p.fail("DATABASE_URL is required when STORAGE=postgres")
		}
	default:
		p.fail("STORAGE must be %q or %q, got %q", StorageMemory, StoragePostgres, cfg.Storage)
	}

	if cfg.DB.MaxConns < 1 {
		p.fail("DB_MAX_CONNS must be >= 1")
	}
	if cfg.DB.MinConns < 0 || cfg.DB.MinConns > cfg.DB.MaxConns {
		p.fail("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS")
	}
	if cfg.RequestTimeout <= 0 {
		p.fail("REQUEST_TIMEOUT must be > 0")
	}
	if cfg.HTTPIdleTimeout <= 0 {
		p.fail("HTTP_IDLE_TIMEOUT must be > 0")
	}
	if cfg.ShutdownDelay < 0 || cfg.ShutdownTimeout < 0 {
		p.fail("SHUTDOWN_DELAY and SHUTDOWN_TIMEOUT must be >= 0")
	}

	return cfg, errors.Join(p.errs...)
}

func FromEnv() (Config, error) { return Load(os.LookupEnv) }

// parser gom lỗi của mọi biến để báo một lần, thay vì dừng ở lỗi đầu tiên.
type parser struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

func (p *parser) str(key, def string) string {
	if v, ok := p.lookup(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (p *parser) duration(key, def string) time.Duration {
	d, err := time.ParseDuration(p.str(key, def))
	if err != nil {
		p.fail("%s: %w", key, err)
	}
	return d
}

func (p *parser) int32(key string, def int32) int32 {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		p.fail("%s: %w", key, err)
	}
	return int32(n)
}

func (p *parser) bool(key string, def bool) bool {
	raw := p.str(key, "")
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		p.fail("%s: %w", key, err)
	}
	return b
}

func (p *parser) cidrs(key string) []*net.IPNet {
	var out []*net.IPNet
	for _, part := range strings.Split(p.str(key, ""), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			p.fail("%s: %w", key, err)
			continue
		}
		out = append(out, n)
	}
	return out
}
