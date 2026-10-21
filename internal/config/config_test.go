package config_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/example/userapi/internal/config"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := config.Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.Storage != config.StorageMemory || cfg.LogLevel != slog.LevelInfo || cfg.ShutdownTimeout != 10*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.ShutdownDelay != 0 || cfg.ReadinessCheckDB || cfg.APIDocsEnabled || len(cfg.TrustedProxies) != 0 || cfg.HTTPIdleTimeout != 75*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.DB.MaxConns != 10 || cfg.DB.StatementTimeout != 5*time.Second || cfg.DB.MigrateOnStart {
		t.Fatalf("db defaults: %+v", cfg.DB)
	}
	ev := cfg.Events
	if ev.Sink != config.EventsSinkNone || ev.KafkaTopic != "users" || ev.PollInterval != time.Second ||
		ev.BatchSize != 100 || ev.Retention != 7*24*time.Hour || len(ev.KafkaBrokers) != 0 || ev.RelayHealthAddr != ":8081" {
		t.Fatalf("events defaults: %+v", ev)
	}
}

func TestLoad_Errors(t *testing.T) {
	cases := []map[string]string{
		{"STORAGE": "postgres"},
		{"STORAGE": "mongo"},
		{"LOG_LEVEL": "loud"},
		{"SHUTDOWN_TIMEOUT": "ten"},
		{"SHUTDOWN_DELAY": "-1s"},
		{"REQUEST_TIMEOUT": "0s"},
		{"HTTP_IDLE_TIMEOUT": "0s"},
		{"DB_MAX_CONNS": "0"},
		{"DB_MAX_CONNS": "many"},
		{"DB_MIN_CONNS": "20"},
		{"READINESS_CHECK_DB": "maybe"},
		{"API_DOCS_ENABLED": "yes"},
		{"TRUSTED_PROXIES": "10.0.0.0/8,not-a-cidr"},
		{"EVENTS_SINK": "rabbitmq"},
		// statement_timeout uses milliseconds: values below 1ms become 0, DISABLING the server timeout.
		{"DB_STATEMENT_TIMEOUT": "500us"},
		// The outbox lives in PostgreSQL: events require STORAGE=postgres.
		{"EVENTS_SINK": "kafka", "KAFKA_BROKERS": "localhost:9092"},
		{"EVENTS_SINK": "kafka", "STORAGE": "postgres", "DATABASE_URL": "postgres://x"},
		{"OUTBOX_BATCH_SIZE": "0"},
		{"OUTBOX_BATCH_SIZE": "1001"},
		{"OUTBOX_POLL_INTERVAL": "0s"},
		{"OUTBOX_RETENTION": "0s"},
		{"KAFKA_SASL_MECHANISM": "GSSAPI"},
		{"KAFKA_SASL_MECHANISM": "SCRAM-SHA-512"}, // Missing username/password.
		{"KAFKA_SASL_MECHANISM": "SCRAM-SHA-512", "KAFKA_SASL_USERNAME": "u", "KAFKA_SASL_PASSWORD": "   "}, // Blank password counts as missing.
		{"KAFKA_SASL_MECHANISM": "PLAIN", "KAFKA_SASL_USERNAME": "u", "KAFKA_SASL_PASSWORD": "p"},           // PLAIN sends the password in cleartext and requires TLS.
		{"KAFKA_TLS_CA_FILE": "/etc/ca.pem"}, // CA configured without TLS.
		{"KAFKA_TLS": "maybe"},
	}
	for _, c := range cases {
		if _, err := config.Load(env(c)); err == nil {
			t.Errorf("expected error for %v", c)
		}
	}
}

func TestLoad_Postgres(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"STORAGE":          "Postgres",
		"DATABASE_URL":     "postgres://x",
		"LOG_LEVEL":        "debug",
		"DB_MAX_CONNS":     "25",
		"MIGRATE_ON_START": "true",
		"TRUSTED_PROXIES":  "10.0.0.0/8, 192.168.1.0/24",
	}))
	if err != nil || cfg.Storage != config.StoragePostgres || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("%+v %v", cfg, err)
	}
	if cfg.DB.MaxConns != 25 || !cfg.DB.MigrateOnStart || len(cfg.TrustedProxies) != 2 {
		t.Fatalf("%+v", cfg)
	}
}

func TestLoad_KafkaEvents(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"STORAGE":              "postgres",
		"DATABASE_URL":         "postgres://x",
		"EVENTS_SINK":          "Kafka",
		"KAFKA_BROKERS":        "kafka-0:9092, kafka-1:9092",
		"KAFKA_TOPIC":          "userapi.users",
		"OUTBOX_POLL_INTERVAL": "250ms",
		"OUTBOX_BATCH_SIZE":    "500",
		"OUTBOX_RETENTION":     "72h",
	}))
	if err != nil {
		t.Fatal(err)
	}
	ev := cfg.Events
	if ev.Sink != config.EventsSinkKafka || ev.KafkaTopic != "userapi.users" || ev.PollInterval != 250*time.Millisecond ||
		ev.BatchSize != 500 || ev.Retention != 72*time.Hour {
		t.Fatalf("%+v", ev)
	}
	if len(ev.KafkaBrokers) != 2 || ev.KafkaBrokers[0] != "kafka-0:9092" || ev.KafkaBrokers[1] != "kafka-1:9092" {
		t.Fatalf("brokers = %q", ev.KafkaBrokers)
	}
	if ev.KafkaTLS || ev.KafkaSASLMechanism != "" {
		t.Fatalf("TLS and SASL must default to off: %+v", ev)
	}
}

func TestLoad_KafkaSecurity(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": "postgres://x", "EVENTS_SINK": "kafka", "KAFKA_BROKERS": "b:9096",
		"KAFKA_TLS": "true", "KAFKA_TLS_CA_FILE": "/etc/kafka/ca.pem",
		"KAFKA_SASL_MECHANISM": "scram-sha-512", "KAFKA_SASL_USERNAME": "relay", "KAFKA_SASL_PASSWORD": " pw ", // Credentials are used verbatim.
	}))
	if err != nil {
		t.Fatal(err)
	}
	ev := cfg.Events
	if !ev.KafkaTLS || ev.KafkaTLSCAFile != "/etc/kafka/ca.pem" || ev.KafkaSASLMechanism != "SCRAM-SHA-512" ||
		ev.KafkaSASLUsername != "relay" || ev.KafkaSASLPassword != " pw " {
		t.Fatalf("%+v", ev)
	}
}

func TestLoad_APIDocsEnabled(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"API_DOCS_ENABLED": "true"}))
	if err != nil || !cfg.APIDocsEnabled {
		t.Fatalf("APIDocsEnabled = %v, err = %v", cfg.APIDocsEnabled, err)
	}
}
