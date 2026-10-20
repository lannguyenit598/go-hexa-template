// Package config reads configuration from environment variables (12-factor).
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

// EventsSink is the relay destination. "none" disables both outbox writes and event publishing.
type EventsSink string

const (
	EventsSinkNone  EventsSink = "none"
	EventsSinkKafka EventsSink = "kafka"
)

type Config struct {
	HTTPAddr string
	Storage  Storage
	LogLevel slog.Level

	// ShutdownDelay waits after SIGTERM before stopping new connections, allowing
	// the load balancer / kube-proxy to remove the pod from its endpoints.
	ShutdownDelay   time.Duration
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration
	// HTTPIdleTimeout keeps idle keep-alive connections open. Must exceed the upstream
	// load balancer idle timeout (AWS ALB: 60s) to avoid intermittent 502s.
	HTTPIdleTimeout time.Duration

	// TrustedProxies lists trusted proxy CIDRs. Empty = use the TCP connection IP.
	TrustedProxies []*net.IPNet

	// ReadinessCheckDB controls /readyz database pings. Disabled by default so a brief database
	// outage does not remove EVERY pod from the load balancer simultaneously.
	ReadinessCheckDB bool

	DB     DB
	Events Events
}

// DB configures the PostgreSQL connection pool.
type DB struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	MaxConnLifetime  time.Duration
	MaxConnIdleTime  time.Duration
	StatementTimeout time.Duration
	// MigrateOnStart runs migrations at startup for development. In production, run
	// `userapi migrate` as a separate Job / initContainer (see docs/operations.md).
	MigrateOnStart bool
}

// Events configures transactional outbox publishing (see docs/events.md).
type Events struct {
	Sink         EventsSink
	KafkaBrokers []string
	KafkaTopic   string
	// KafkaTLS encrypts broker connections; KafkaTLSCAFile (PEM) replaces system CAs for a private broker CA.
	KafkaTLS       bool
	KafkaTLSCAFile string
	// KafkaSASLMechanism: "" (no authentication), PLAIN, SCRAM-SHA-256, or SCRAM-SHA-512.
	KafkaSASLMechanism string
	KafkaSASLUsername  string
	KafkaSASLPassword  string
	// PollInterval is the wait between outbox reads when there is no backlog.
	PollInterval time.Duration
	BatchSize    int
	// Retention controls how long published outbox rows remain before the relay deletes them.
	Retention time.Duration
	// RelayHealthAddr is the `userapi relay` liveness HTTP address (GET /healthz).
	RelayHealthAddr string
}

// Load reads env through a replaceable lookup (os.LookupEnv in production, a map in tests).
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
	// statement_timeout is sent in milliseconds: 0 < value < 1ms becomes 0, disabling the server timeout.
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

// parser collects errors from all variables instead of stopping at the first error.
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

// list splits comma-separated values and discards empty entries.
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
