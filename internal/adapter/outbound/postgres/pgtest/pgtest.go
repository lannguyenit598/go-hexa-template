// Package pgtest provides PostgreSQL integration test helpers. For tests only.
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

// FreshSchemaDSN creates an empty schema named "t_<unixnano>" and returns a DSN pointing
// search_path to it, giving each test a fresh database view without affecting other tests.
// t.Cleanup runs DROP SCHEMA ... CASCADE. Skip if TEST_DATABASE_URL is empty.
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
	q.Set("search_path", schema) // pgx passes unknown DSN parameters as runtime parameters.
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
