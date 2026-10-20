package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/postgres/pgtest"
	"github.com/example/userapi/internal/adapter/outbound/repotest"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
)

// Integration test: runs only with TEST_DATABASE_URL, for example
//
//	TEST_DATABASE_URL=postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable go test ./...
//
// WARNING: this test TRUNCATEs users and outbox. Never point it at a real application database.
func TestUserRepositoryContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := postgres.Migrate(ctx, dsn, quiet); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 4, StatementTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if pending, err := postgres.PendingMigrations(ctx, pool); err != nil || pending {
		t.Fatalf("pending migrations after Migrate: %v %v", pending, err)
	}

	// Writing the outbox in the same transaction must not change the repository contract.
	for _, tc := range []struct {
		name   string
		outbox bool
	}{{"without outbox", false}, {"with outbox", true}} {
		t.Run(tc.name, func(t *testing.T) {
			repotest.Run(t, func(t *testing.T) port.UserRepository {
				if _, err := pool.Exec(ctx, `TRUNCATE users, outbox`); err != nil {
					t.Fatal(err)
				}
				return postgres.NewUserRepository(pool, tc.outbox)
			})
		})
	}

	t.Run("concurrent updates are never lost", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE users`); err != nil {
			t.Fatal(err)
		}
		svc := service.NewUserService(postgres.NewUserRepository(pool, false), system.UUIDv7{}, system.Clock{}, nil)
		u, err := svc.Register(ctx, port.RegisterUserCmd{Email: "race@example.com", Name: "Race"})
		if err != nil {
			t.Fatal(err)
		}

		const workers = 20
		var (
			wg        sync.WaitGroup
			successes atomic.Int64
		)
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				name := fmt.Sprintf("Worker %d", i)
				_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name})
				switch {
				case err == nil:
					successes.Add(1)
				case !errors.Is(err, port.ErrVersionConflict):
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		got, err := svc.Get(ctx, u.ID.String())
		if err != nil {
			t.Fatal(err)
		}
		// Each successful write increments version by exactly 1: no write is silently overwritten.
		if got.Version != 1+successes.Load() {
			t.Fatalf("version = %d, successes = %d", got.Version, successes.Load())
		}
		t.Logf("%d/%d updates succeeded, the rest got ErrVersionConflict", successes.Load(), workers)
	})
}

// statement_timeout (DB_STATEMENT_TIMEOUT) bounds request execution at the database:
// the error must match context.DeadlineExceeded so HTTP returns 503 rather than 500.
func TestStatementTimeout_IsDeadlineExceeded(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 2, StatementTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	// Lock the table in another transaction so the repository query waits until timeout.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	_, _, err = postgres.NewUserRepository(pool, false).List(ctx, 10, 0)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List = %v, want context.DeadlineExceeded", err)
	}
}

func TestPendingMigrations_EmptySchema(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	pending, err := postgres.PendingMigrations(ctx, pool)
	if err != nil || !pending {
		t.Fatalf("empty database: pending=%v err=%v, want pending=true err=nil", pending, err)
	}
	// The check must be read-only: application roles often lack DDL privileges, so do not create
	// goose's version table (a DML-only role would get a confusing "permission denied").
	var created bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("PendingMigrations must not create goose_db_version")
	}
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if pending, err := postgres.PendingMigrations(ctx, pool); err != nil || pending {
		t.Fatalf("after migrate: pending=%v err=%v", pending, err)
	}
}
