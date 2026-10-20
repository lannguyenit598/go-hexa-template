package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embedded embed.FS

// migrationLockTimeout sets lock_timeout for migration sessions. ALTER TABLE waits for
// ACCESS EXCLUSIVE; while waiting, EVERY other query on the table queues behind it.
// Fail migrations early and retry later rather than blocking all traffic.
const migrationLockTimeout = 5 * time.Second

func migrationsFS() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // Only an incorrect embed path can cause this programming error.
	}
	return sub
}

// Migrate applies pending migrations using goose:
//   - goose_db_version tracks applied versions; each file runs exactly once.
//   - An advisory lock on ONE connection, managed by goose, prevents concurrent replicas
//     or Jobs from running migrations over one another.
//   - Each file runs in its own transaction unless marked "-- +goose NO TRANSACTION"
//     (required for CREATE INDEX CONCURRENTLY).
//
// Migrations use a separate connection with statement_timeout = 0 (index creation may be slow)
// and a short lock_timeout, NOT the application pool.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	db, err := openMigrationDB(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS(),
		goose.WithSessionLocker(locker),
		goose.WithSlog(logger),
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		logger.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration.String())
	}
	return nil
}

// PendingMigrations reports whether migrations remain. Startup uses it to fail early with a
// clear message rather than starting and later returning "column does not exist".
//
// This function is read-only. goose.HasPending creates goose_db_version if absent, running DDL
// as the application role; a DML-only role (recommended for production) would receive
// "permission denied". Detect an unmigrated database first with to_regclass
// (respecting search_path) and treat it as having pending migrations.
func PendingMigrations(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var hasVersionTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&hasVersionTable); err != nil {
		return false, fmt.Errorf("check migration table: %w", err)
	}
	if !hasVersionTable {
		return true, nil
	}

	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }() // Close only the wrapper, not the pool.
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS())
	if err != nil {
		return false, fmt.Errorf("create migration provider: %w", err)
	}
	return provider.HasPending(ctx)
}

func openMigrationDB(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.RuntimeParams["statement_timeout"] = "0"
	cfg.RuntimeParams["lock_timeout"] = fmt.Sprint(migrationLockTimeout.Milliseconds())
	cfg.RuntimeParams["application_name"] = "userapi-migrate"
	return stdlib.OpenDB(*cfg), nil
}
