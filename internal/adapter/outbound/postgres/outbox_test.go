package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/postgres/pgtest"
	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/outbox"
)

// freshPool trả pool trên một schema riêng đã migrate (skip nếu không có TEST_DATABASE_URL).
func freshPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	pool, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type outboxRow struct {
	ID, AggregateID, EventType string
	Payload                    map[string]any
	Headers                    map[string]string
}

func outboxRows(t *testing.T, pool *pgxpool.Pool) []outboxRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT id::text, aggregate_id::text, event_type, payload, headers FROM outbox ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		if err := rows.Scan(&r.ID, &r.AggregateID, &r.EventType, &r.Payload, &r.Headers); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func newUser(t *testing.T, id, email string) domain.User {
	t.Helper()
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	u, err := domain.NewUser(domain.UserID(id), email, "Outbox User", now)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

const (
	userA = "01a15359-aa28-7a00-a380-c47e52905707"
	userB = "01a15359-aa28-7a00-a380-c47e52905708"
)

// Mỗi lần ghi THÀNH CÔNG sinh đúng một sự kiện; lần ghi thất bại không sinh sự kiện nào.
func TestOutbox_OneEventPerSuccessfulWrite(t *testing.T) {
	pool := freshPool(t)
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	ctx := context.Background()

	u := newUser(t, userA, "a@example.com")
	if err := repo.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, newUser(t, userB, "a@example.com")); !errors.Is(err, domain.ErrEmailAlreadyExists) {
		t.Fatalf("duplicate email: %v", err)
	}
	next, err := u.Apply(domain.Changes{Name: ptr("Renamed")}, u.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, next, u.Version); err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, next, u.Version); !errors.Is(err, port.ErrVersionConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if err := repo.Update(ctx, newUser(t, userB, "b@example.com"), 1); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("missing update: %v", err)
	}
	if err := repo.Delete(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, u.ID); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}

	rows := outboxRows(t, pool)
	want := []struct {
		typ     string
		version float64
	}{{"user.registered", 1}, {"user.updated", 2}, {"user.deleted", 2}}
	if len(rows) != len(want) {
		t.Fatalf("got %d outbox rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		r := rows[i]
		p := r.Payload
		if r.EventType != w.typ || r.AggregateID != userA || p["event_type"] != w.typ || p["user_id"] != userA ||
			p["version"] != w.version || p["event_id"] != r.ID || p["schema_version"] != float64(1) {
			t.Fatalf("row %d = %+v, want %s v%v", i, r, w.typ, w.version)
		}
		if _, err := time.Parse(time.RFC3339Nano, p["occurred_at"].(string)); err != nil {
			t.Fatalf("occurred_at: %v", err)
		}
		// Sự kiện mỏng: không mang PII.
		for _, k := range []string{"email", "name"} {
			if _, ok := p[k]; ok {
				t.Fatalf("payload must not contain %q: %v", k, p)
			}
		}
	}
	if occurred := rows[1].Payload["occurred_at"]; occurred != next.UpdatedAt.Format(time.RFC3339Nano) {
		t.Fatalf("user.updated occurred_at = %v, want the update time %v", occurred, next.UpdatedAt)
	}
}

func TestOutbox_DisabledWritesNothing(t *testing.T) {
	pool := freshPool(t)
	if err := postgres.NewUserRepository(pool).Create(context.Background(), newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, pool); len(rows) != 0 {
		t.Fatalf("outbox rows written without WithOutbox: %+v", rows)
	}
}

// Ghi outbox thất bại thì thay đổi dữ liệu cũng không được commit: không bao giờ có dữ liệu
// đổi mà thiếu sự kiện.
func TestOutbox_IsAtomicWithTheWrite(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE outbox`); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); err == nil {
		t.Fatal("Create succeeded without an outbox table")
	}
	if _, err := repo.GetByID(ctx, userA); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("user was committed although its event was not: %v", err)
	}
}

func TestOutbox_CarriesTraceContext(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	pool := freshPool(t)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	if err := postgres.NewUserRepository(pool, postgres.WithOutbox()).Create(ctx, newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)
	if len(rows) != 1 || !strings.Contains(rows[0].Headers["traceparent"], "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("headers = %+v, want the request's traceparent", rows)
	}
}

func TestOutboxSource_ProcessInOrderAndOnlyMarksOnSuccess(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	for _, u := range []domain.User{newUser(t, userA, "a@example.com"), newUser(t, userB, "b@example.com")} {
		if err := repo.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Delete(ctx, userA); err != nil {
		t.Fatal(err)
	}
	src := postgres.NewOutboxSource(pool)

	// Broker lỗi: không message nào bị đánh dấu.
	if _, err := src.Process(ctx, 10, func(context.Context, []outbox.Message) error { return errors.New("down") }); err == nil {
		t.Fatal("want publish error")
	}

	var got []outbox.Message
	collect := func(_ context.Context, msgs []outbox.Message) error { got = append(got, msgs...); return nil }
	for _, want := range []int{2, 1, 0} {
		n, err := src.Process(ctx, 2, collect)
		if err != nil || n != want {
			t.Fatalf("Process = %d, %v; want %d", n, err, want)
		}
	}
	if len(got) != 3 {
		t.Fatalf("published %d messages, want 3", len(got))
	}
	for i, w := range []struct{ key, typ string }{{userA, "user.registered"}, {userB, "user.registered"}, {userA, "user.deleted"}} {
		m := got[i]
		var p map[string]any
		if err := json.Unmarshal(m.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if m.Key != w.key || m.Type != w.typ || p["event_id"] != m.ID || m.Headers == nil {
			t.Fatalf("message %d = %+v, want key %s type %s", i, m, w.key, w.typ)
		}
	}
}

// Hai relay chạy song song không bao giờ lấy cùng một message (FOR UPDATE SKIP LOCKED).
func TestOutboxSource_ConcurrentRelaysDoNotShareMessages(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	for _, u := range []domain.User{newUser(t, userA, "a@example.com"), newUser(t, userB, "b@example.com")} {
		if err := repo.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	src := postgres.NewOutboxSource(pool)

	var mu sync.Mutex
	var ids []string
	entered, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := src.Process(ctx, 1, func(_ context.Context, msgs []outbox.Message) error {
			mu.Lock()
			ids = append(ids, msgs[0].ID)
			mu.Unlock()
			close(entered)
			<-release // giữ khoá trên message đầu tiên
			return nil
		})
		first <- err
	}()
	<-entered
	// Có timeout: nếu thiếu SKIP LOCKED, relay thứ hai sẽ chờ khoá mãi thay vì lấy message kế tiếp.
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := src.Process(ctx2, 1, func(_ context.Context, msgs []outbox.Message) error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, msgs[0].ID)
		return nil
	})
	close(release)
	if err != nil || n != 1 {
		t.Fatalf("second relay: n=%d err=%v", n, err)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("relays published %v, want two different messages", ids)
	}
}

func TestOutboxSource_PurgeDeletesOnlyPublished(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	src := postgres.NewOutboxSource(pool)
	if _, err := src.Process(ctx, 10, func(context.Context, []outbox.Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, newUser(t, userB, "b@example.com")); err != nil { // chưa publish
		t.Fatal(err)
	}
	if n, err := src.Purge(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("purge before publish time: n=%d err=%v, want 0", n, err)
	}
	if n, err := src.Purge(ctx, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v, want 1", n, err)
	}
	if rows := outboxRows(t, pool); len(rows) != 1 || rows[0].AggregateID != userB {
		t.Fatalf("remaining rows %+v, want only the unpublished one", rows)
	}
}

func ptr(s string) *string { return &s }
