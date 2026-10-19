// Package config đọc cấu hình từ biến môi trường (12-factor).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Storage string

const (
	StorageMemory   Storage = "memory"
	StoragePostgres Storage = "postgres"
)

type Config struct {
	HTTPAddr        string
	Storage         Storage
	DatabaseURL     string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	RequestTimeout  time.Duration
}

// Load đọc env với lookup có thể thay thế (os.LookupEnv khi chạy thật, map khi test).
func Load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return def
	}

	cfg := Config{
		HTTPAddr:    get("HTTP_ADDR", ":8080"),
		Storage:     Storage(strings.ToLower(get("STORAGE", string(StorageMemory)))),
		DatabaseURL: get("DATABASE_URL", ""),
	}

	var errs []error
	switch cfg.Storage {
	case StorageMemory:
	case StoragePostgres:
		if cfg.DatabaseURL == "" {
			errs = append(errs, errors.New("DATABASE_URL is required when STORAGE=postgres"))
		}
	default:
		errs = append(errs, fmt.Errorf("STORAGE must be %q or %q, got %q", StorageMemory, StoragePostgres, cfg.Storage))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}

	var err error
	if cfg.ShutdownTimeout, err = time.ParseDuration(get("SHUTDOWN_TIMEOUT", "10s")); err != nil {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT: %w", err))
	}
	if cfg.RequestTimeout, err = time.ParseDuration(get("REQUEST_TIMEOUT", "10s")); err != nil {
		errs = append(errs, fmt.Errorf("REQUEST_TIMEOUT: %w", err))
	}

	return cfg, errors.Join(errs...)
}

func FromEnv() (Config, error) { return Load(os.LookupEnv) }
