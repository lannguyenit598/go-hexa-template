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

// Integration test: chỉ chạy khi có TEST_DATABASE_URL, ví dụ
//
//	TEST_DATABASE_URL=postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable go test ./...
//
// CẢNH BÁO: test sẽ TRUNCATE bảng users. Đừng trỏ vào database thật.
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

	repotest.Run(t, func(t *testing.T) port.UserRepository {
		if _, err := pool.Exec(ctx, `TRUNCATE users`); err != nil {
			t.Fatal(err)
		}
		return postgres.NewUserRepository(pool)
	})

	t.Run("concurrent updates are never lost", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE users`); err != nil {
			t.Fatal(err)
		}
		svc := service.NewUserService(postgres.NewUserRepository(pool), system.NewUUIDv7(), system.Clock{}, nil)
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
		// Mỗi lần ghi thành công tăng version đúng 1: không lần ghi nào bị ghi đè âm thầm.
		if got.Version != 1+successes.Load() {
			t.Fatalf("version = %d, successes = %d", got.Version, successes.Load())
		}
		t.Logf("%d/%d updates succeeded, the rest got ErrVersionConflict", successes.Load(), workers)
	})
}

// statement_timeout (DB_STATEMENT_TIMEOUT) là giới hạn thời gian của request ở phía database:
// lỗi phải được nhận ra là context.DeadlineExceeded để HTTP trả 503, không phải 500.
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

	// Giữ khoá bảng trong một transaction khác để truy vấn của repository phải chờ tới khi hết hạn.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	_, _, err = postgres.NewUserRepository(pool).List(ctx, 10, 0)
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
	// Kiểm tra phải chỉ đọc: app thường chạy bằng role không có quyền DDL, nên không được tự tạo
	// bảng version của goose (nếu tạo, role chỉ có DML sẽ gặp "permission denied" khó hiểu).
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
