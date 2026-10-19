package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/repotest"
	"github.com/example/userapi/internal/core/port"
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
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	repotest.Run(t, func(t *testing.T) port.UserRepository {
		if _, err := pool.Exec(ctx, `TRUNCATE users`); err != nil {
			t.Fatal(err)
		}
		return postgres.NewUserRepository(pool)
	})
}
