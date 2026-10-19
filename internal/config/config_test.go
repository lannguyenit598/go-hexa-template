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
	if cfg.ShutdownDelay != 0 || cfg.ReadinessCheckDB || len(cfg.TrustedProxies) != 0 || cfg.HTTPIdleTimeout != 75*time.Second {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.DB.MaxConns != 10 || cfg.DB.StatementTimeout != 5*time.Second || cfg.DB.MigrateOnStart {
		t.Fatalf("db defaults: %+v", cfg.DB)
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
		{"TRUSTED_PROXIES": "10.0.0.0/8,not-a-cidr"},
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
