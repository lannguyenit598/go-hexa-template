// Package pgtest chứa helper cho integration test với PostgreSQL. Chỉ dùng trong test.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// FreshSchemaDSN tạo schema rỗng tên "t_<unixnano>" và trả DSN có search_path trỏ vào đó, để
// test chạy trên một database "mới tinh" mà không ảnh hưởng dữ liệu của test khác.
// t.Cleanup sẽ DROP SCHEMA ... CASCADE. Skip test nếu TEST_DATABASE_URL trống.
func FreshSchemaDSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping PostgreSQL integration test")
	}
	schema := fmt.Sprintf("t_%d", time.Now().UnixNano())
	exec(t, base, "CREATE SCHEMA "+schema)
	t.Cleanup(func() { exec(t, base, "DROP SCHEMA IF EXISTS "+schema+" CASCADE") })

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema) // pgx đưa tham số lạ trong DSN thành runtime param
	u.RawQuery = q.Encode()
	return u.String()
}

func exec(t testing.TB, dsn, sql string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
