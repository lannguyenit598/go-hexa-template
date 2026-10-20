package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/example/userapi/internal/adapter/outbound/postgres"
	"github.com/example/userapi/internal/adapter/outbound/postgres/pgtest"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
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
// version của các sự kiện của một user tăng ngặt (user.deleted = version cuối + 1), để consumer
// bỏ bản trùng hoặc bản cũ bằng "version không lớn hơn version đã xử lý" mà không bỏ mất lệnh xoá.
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
	}{{"user.registered", 1}, {"user.updated", 2}, {"user.deleted", 3}}
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

// Header của sự kiện chỉ mang trace context (traceparent/tracestate). Baggage do client tự đặt
// (có thể chứa email, tên, giá trị tuỳ ý) không được lưu vào outbox hay gửi lên broker.
func TestOutbox_CarriesTraceContextButNotBaggage(t *testing.T) {
	// Như production: observability.Setup đặt propagator gồm cả TraceContext và Baggage.
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	pool := freshPool(t)
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	member, err := baggage.NewMember("user.email", "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	bag, err := baggage.New(member)
	if err != nil {
		t.Fatal(err)
	}
	ctx = baggage.ContextWithBaggage(ctx, bag)
	if err := postgres.NewUserRepository(pool, postgres.WithOutbox()).Create(ctx, newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)
	if len(rows) != 1 || !strings.Contains(rows[0].Headers["traceparent"], "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("headers = %+v, want the request's traceparent", rows)
	}
	if b, ok := rows[0].Headers["baggage"]; ok {
		t.Fatalf("baggage stored in the outbox: %q", b)
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
	res, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error {
		return allErr(msgs, errors.New("down"))
	})
	if err != nil || res.Retry != 3 || res.Published != 0 || res.Err == nil {
		t.Fatalf("Process with broker down = %+v, %v; want 3 to retry", res, err)
	}

	var got []outbox.Message
	collect := func(_ context.Context, msgs []outbox.Message) []error {
		got = append(got, msgs...)
		return allErr(msgs, nil)
	}
	for _, want := range []int{2, 1, 0} {
		res, err := src.Process(ctx, 2, collect)
		if err != nil || res.Published != want {
			t.Fatalf("Process = %+v, %v; want %d published", res, err, want)
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
		_, err := src.Process(ctx, 1, func(_ context.Context, msgs []outbox.Message) []error {
			mu.Lock()
			ids = append(ids, msgs[0].ID)
			mu.Unlock()
			close(entered)
			<-release // giữ khoá trên message đầu tiên
			return allErr(msgs, nil)
		})
		first <- err
	}()
	<-entered
	// Có timeout: nếu thiếu SKIP LOCKED, relay thứ hai sẽ chờ khoá mãi thay vì lấy message kế tiếp.
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	res, err := src.Process(ctx2, 1, func(_ context.Context, msgs []outbox.Message) []error {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, msgs[0].ID)
		return allErr(msgs, nil)
	})
	close(release)
	if err != nil || res.Published != 1 {
		t.Fatalf("second relay: %+v err=%v", res, err)
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
	if _, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error { return allErr(msgs, nil) }); err != nil {
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

// allErr trả cùng một kết quả cho mọi message (nil = broker đã nhận tất cả).
func allErr(msgs []outbox.Message, err error) []error {
	errs := make([]error, len(msgs))
	for i := range errs {
		errs[i] = err
	}
	return errs
}

// Kết quả từng message: thành công được đánh dấu, bị từ chối vĩnh viễn được tách riêng (không bao
// giờ được lấy lại, không bị purge), lỗi tạm thời ở lại hàng đợi.
func TestOutboxSource_MarksEachMessageByItsOwnResult(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	const userC = "01a15359-aa28-7a00-a380-c47e52905709"
	for _, u := range []domain.User{newUser(t, userA, "a@example.com"), newUser(t, userB, "b@example.com"), newUser(t, userC, "c@example.com")} {
		if err := repo.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	src := postgres.NewOutboxSource(pool)
	res, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error {
		errs := make([]error, len(msgs))
		for i, m := range msgs {
			switch m.Key {
			case userB:
				errs[i] = fmt.Errorf("MESSAGE_TOO_LARGE: %w", outbox.ErrPermanent)
			case userC:
				errs[i] = errors.New("NOT_LEADER_FOR_PARTITION")
			}
		}
		return errs
	})
	if err != nil || res.Published != 1 || len(res.Parked) != 1 || res.Retry != 1 {
		t.Fatalf("Process = %+v, %v; want 1 published, 1 parked, 1 retry", res, err)
	}

	var parkedErr string
	if err := pool.QueryRow(ctx, `SELECT last_error FROM outbox WHERE aggregate_id = $1 AND parked_at IS NOT NULL`, userB).Scan(&parkedErr); err != nil {
		t.Fatalf("parked row: %v", err)
	}
	if !strings.Contains(parkedErr, "MESSAGE_TOO_LARGE") {
		t.Fatalf("last_error = %q", parkedErr)
	}

	// Lần sau chỉ còn message lỗi tạm thời; message đã tách riêng không quay lại.
	var keys []string
	if _, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error {
		for _, m := range msgs {
			keys = append(keys, m.Key)
		}
		return allErr(msgs, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "["+userC+"]" {
		t.Fatalf("second batch %v, want only the retried message", keys)
	}
	if n, err := src.Purge(ctx, time.Now().Add(time.Hour)); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v; want the 2 published rows, not the parked one", n, err)
	}
}

// pauseDelete giữ câu DELETE ở phía client cho tới khi test cho phép chạy tiếp: transaction của
// Delete đã BEGIN nhưng chưa giữ khoá dòng, đúng khoảng hở mà một Update khác có thể commit trước.
type pauseDelete struct {
	reached, release chan struct{}
}

func (p pauseDelete) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "DELETE FROM users") {
		close(p.reached)
		<-p.release
	}
	return ctx
}

func (pauseDelete) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Thứ tự sự kiện của một user phải theo thứ tự commit, kể cả khi transaction của Delete bắt đầu
// trước một Update commit trước nó (relay sắp xếp theo thời điểm ghi bản ghi outbox).
func TestOutbox_EventOrderFollowsCommitOrder(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pause := pauseDelete{reached: make(chan struct{}), release: make(chan struct{})}
	cfg.ConnConfig.Tracer = pause
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())

	u := newUser(t, userA, "order@example.com")
	if err := repo.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- repo.Delete(ctx, u.ID) }()
	<-pause.reached // Delete đã BEGIN, chưa chạy DELETE
	next, err := u.Apply(domain.Changes{Name: ptr("Renamed")}, u.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, next, u.Version); err != nil { // commit trước Delete
		t.Fatal(err)
	}
	close(pause.release)
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}

	var got []string
	if _, err := postgres.NewOutboxSource(pool).Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error {
		for _, m := range msgs {
			got = append(got, m.Type)
		}
		return allErr(msgs, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != "[user.registered user.updated user.deleted]" {
		t.Fatalf("relay order %v, want registered, updated, deleted", got)
	}
}

// Thao tác qua service không làm dữ liệu đổi thì không sinh sự kiện: PATCH giữ nguyên giá trị,
// If-Match lệch version (412), dữ liệu không hợp lệ (422), email trùng (409).
func TestOutbox_NoEventWhenNothingChanges(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	svc := service.NewUserService(repo, system.NewUUIDv7(), system.Clock{}, nil)
	u, err := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "Same"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Register(ctx, port.RegisterUserCmd{Email: "b@example.com", Name: "Other"}); err != nil {
		t.Fatal(err)
	}
	before := len(outboxRows(t, pool))

	same, bad, taken := "Same", "not-an-email", "b@example.com"
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &same}); err != nil {
		t.Fatalf("no-op update: %v", err)
	}
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: ptr("New"), ExpectedVersion: 99}); !errors.Is(err, port.ErrVersionConflict) {
		t.Fatalf("stale If-Match: %v", err)
	}
	var ve *domain.ValidationError
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Email: &bad}); !errors.As(err, &ve) {
		t.Fatalf("invalid email: %v", err)
	}
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Email: &taken}); !errors.Is(err, domain.ErrEmailAlreadyExists) {
		t.Fatalf("duplicate email: %v", err)
	}
	if after := len(outboxRows(t, pool)); after != before {
		t.Fatalf("outbox rows %d -> %d, want no new event", before, after)
	}
}

// Update và Delete cũng nguyên tử với sự kiện của chúng (Create được kiểm ở TestOutbox_IsAtomicWithTheWrite).
func TestOutbox_UpdateAndDeleteAreAtomicWithTheirEvents(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, postgres.WithOutbox())
	u := newUser(t, userA, "a@example.com")
	if err := repo.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE outbox`); err != nil {
		t.Fatal(err)
	}
	next, err := u.Apply(domain.Changes{Name: ptr("Renamed")}, u.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, next, u.Version); err == nil {
		t.Fatal("Update succeeded without an outbox table")
	}
	if err := repo.Delete(ctx, u.ID); err == nil {
		t.Fatal("Delete succeeded without an outbox table")
	}
	got, err := repo.GetByID(ctx, u.ID)
	if err != nil || got.Name != u.Name || got.Version != u.Version {
		t.Fatalf("user after failed writes = %+v, %v; want it unchanged", got, err)
	}
}
