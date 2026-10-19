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
	"github.com/example/userapi/internal/adapter/outbound/repotest"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/core/domain"
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
				_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}})
				switch {
				case err == nil:
					successes.Add(1)
				case !errors.Is(err, domain.ErrVersionConflict):
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		got, err := svc.Get(ctx, u.ID)
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
