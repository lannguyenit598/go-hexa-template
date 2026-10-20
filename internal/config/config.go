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

// EventsSink là nơi relay đẩy sự kiện từ outbox tới. "none" = không ghi outbox, không phát sự kiện.
type EventsSink string

const (
	EventsSinkNone  EventsSink = "none"
	EventsSinkKafka EventsSink = "kafka"
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

	DB     DB
	Events Events
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
	// `userapi migrate` như một Job / initContainer riêng (xem docs/operations.md).
	MigrateOnStart bool
}

// Events là cấu hình phát sự kiện qua transactional outbox (xem docs/events.md).
type Events struct {
	Sink         EventsSink
	KafkaBrokers []string
	KafkaTopic   string
	// KafkaTLS mã hoá kết nối tới broker; KafkaTLSCAFile (PEM) thay CA của hệ thống khi broker dùng CA nội bộ.
	KafkaTLS       bool
	KafkaTLSCAFile string
	// KafkaSASLMechanism: "" (không xác thực), PLAIN, SCRAM-SHA-256 hoặc SCRAM-SHA-512.
	KafkaSASLMechanism string
	KafkaSASLUsername  string
	KafkaSASLPassword  string
	// PollInterval: relay chờ bao lâu giữa hai lần đọc outbox khi không còn gì tồn đọng.
	PollInterval time.Duration
	BatchSize    int
	// Retention: bản ghi outbox đã publish được giữ bao lâu trước khi relay xoá.
	Retention time.Duration
	// RelayHealthAddr: địa chỉ HTTP của `userapi relay` cho liveness probe (GET /healthz).
	RelayHealthAddr string
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
		Events: Events{
			Sink:               EventsSink(strings.ToLower(p.str("EVENTS_SINK", string(EventsSinkNone)))),
			KafkaBrokers:       p.list("KAFKA_BROKERS"),
			KafkaTopic:         p.str("KAFKA_TOPIC", "users"),
			KafkaTLS:           p.bool("KAFKA_TLS", false),
			KafkaTLSCAFile:     p.str("KAFKA_TLS_CA_FILE", ""),
			KafkaSASLMechanism: strings.ToUpper(p.str("KAFKA_SASL_MECHANISM", "")),
			KafkaSASLUsername:  p.str("KAFKA_SASL_USERNAME", ""),
			KafkaSASLPassword:  p.str("KAFKA_SASL_PASSWORD", ""),
			PollInterval:       p.duration("OUTBOX_POLL_INTERVAL", "1s"),
			BatchSize:          int(p.int32("OUTBOX_BATCH_SIZE", 100)),
			Retention:          p.duration("OUTBOX_RETENTION", "168h"),
			RelayHealthAddr:    p.str("RELAY_HEALTH_ADDR", ":8081"),
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
	// statement_timeout được gửi theo mili giây: 0 < giá trị < 1ms sẽ thành 0, tức tắt timeout của server.
	if t := cfg.DB.StatementTimeout; t > 0 && t < time.Millisecond {
		p.fail("DB_STATEMENT_TIMEOUT must be 0 or at least 1ms")
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

	switch cfg.Events.Sink {
	case EventsSinkNone:
	case EventsSinkKafka:
		if len(cfg.Events.KafkaBrokers) == 0 {
			p.fail("KAFKA_BROKERS is required when EVENTS_SINK=kafka")
		}
		if cfg.Storage != StoragePostgres {
			p.fail("EVENTS_SINK=%s requires STORAGE=postgres (the outbox lives in PostgreSQL)", cfg.Events.Sink)
		}
	default:
		p.fail("EVENTS_SINK must be %q or %q, got %q", EventsSinkNone, EventsSinkKafka, cfg.Events.Sink)
	}
	switch ev := cfg.Events; ev.KafkaSASLMechanism {
	case "":
	case "PLAIN", "SCRAM-SHA-256", "SCRAM-SHA-512":
		if ev.KafkaSASLUsername == "" || ev.KafkaSASLPassword == "" {
			p.fail("KAFKA_SASL_USERNAME and KAFKA_SASL_PASSWORD are required when KAFKA_SASL_MECHANISM is set")
		}
		if ev.KafkaSASLMechanism == "PLAIN" && !ev.KafkaTLS {
			p.fail("KAFKA_SASL_MECHANISM=PLAIN sends the password in clear text: set KAFKA_TLS=true")
		}
	default:
		p.fail("KAFKA_SASL_MECHANISM must be PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512, got %q", ev.KafkaSASLMechanism)
	}
	if cfg.Events.KafkaTLSCAFile != "" && !cfg.Events.KafkaTLS {
		p.fail("KAFKA_TLS_CA_FILE requires KAFKA_TLS=true")
	}
	if cfg.Events.BatchSize < 1 || cfg.Events.BatchSize > 1000 {
		p.fail("OUTBOX_BATCH_SIZE must be between 1 and 1000")
	}
	if cfg.Events.PollInterval <= 0 || cfg.Events.Retention <= 0 {
		p.fail("OUTBOX_POLL_INTERVAL and OUTBOX_RETENTION must be > 0")
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

// list tách giá trị theo dấu phẩy, bỏ phần tử rỗng.
func (p *parser) list(key string) []string {
	var out []string
	for _, part := range strings.Split(p.str(key, ""), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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
