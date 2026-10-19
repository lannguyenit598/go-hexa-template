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
}

func TestLoad_Errors(t *testing.T) {
	cases := []map[string]string{
		{"STORAGE": "postgres"},
		{"STORAGE": "mongo"},
		{"LOG_LEVEL": "loud"},
		{"SHUTDOWN_TIMEOUT": "ten"},
	}
	for _, c := range cases {
		if _, err := config.Load(env(c)); err == nil {
			t.Errorf("expected error for %v", c)
		}
	}
}

func TestLoad_Postgres(t *testing.T) {
	cfg, err := config.Load(env(map[string]string{"STORAGE": "Postgres", "DATABASE_URL": "postgres://x", "LOG_LEVEL": "debug"}))
	if err != nil || cfg.Storage != config.StoragePostgres || cfg.LogLevel != slog.LevelDebug {
		t.Fatalf("%+v %v", cfg, err)
	}
}
