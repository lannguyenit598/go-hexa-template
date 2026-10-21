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

// freshPool returns a pool on a fresh migrated schema (skips without TEST_DATABASE_URL).
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

// Each SUCCESSFUL write creates exactly one event; failed writes create none.
// Event versions strictly increase per user (user.deleted = last version + 1), letting consumers
// drop versions no greater than the processed version without losing deletions.
func TestOutbox_OneEventPerSuccessfulWrite(t *testing.T) {
	pool := freshPool(t)
	repo := postgres.NewUserRepository(pool, true)
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
		// Thin events: no personal data fields.
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
	if err := postgres.NewUserRepository(pool, false).Create(context.Background(), newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	if rows := outboxRows(t, pool); len(rows) != 0 {
		t.Fatalf("outbox rows written with outbox disabled: %+v", rows)
	}
}

// If writing the outbox fails, the data change must not commit:
// never change data without its event.
func TestOutbox_IsAtomicWithTheWrite(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE outbox`); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewUserRepository(pool, true)
	if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); err == nil {
		t.Fatal("Create succeeded without an outbox table")
	}
	if _, err := repo.GetByID(ctx, userA); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("user was committed although its event was not: %v", err)
	}
}

// Event headers contain only trace context (traceparent/tracestate). Client-supplied baggage
// (which may include email, names, or arbitrary values) must not enter the outbox or broker.
func TestOutbox_CarriesTraceContextButNotBaggage(t *testing.T) {
	// As in production: observability.Setup configures both TraceContext and Baggage propagation.
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
	if err := postgres.NewUserRepository(pool, true).Create(ctx, newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	rows := outboxRows(t, pool)
	if len(rows) != 1 || !strings.Contains(rows[0].Headers["traceparent"], "4bf92f3577b34da6a3ce929d0e0e4736") {
		t.Fatalf("headers = %+v, want the request's traceparent", rows)
	}
	if b, ok := rows[0].Headers["baggage"]; ok {
		t.Fatalf("baggage stored in the outbox: %q", b)
	}
	// The relay reads the same headers back.
	var relayed map[string]string
	if _, err := postgres.NewOutboxSource(pool).Process(context.Background(), 10, func(_ context.Context, msgs []outbox.Message) []error {
		relayed = msgs[0].Headers
		return allErr(msgs, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(relayed) != fmt.Sprint(rows[0].Headers) {
		t.Fatalf("relayed headers %v, want %v", relayed, rows[0].Headers)
	}
}

func TestOutboxSource_ProcessInOrderAndOnlyMarksOnSuccess(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
	for _, u := range []domain.User{newUser(t, userA, "a@example.com"), newUser(t, userB, "b@example.com")} {
		if err := repo.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.Delete(ctx, userA); err != nil {
		t.Fatal(err)
	}
	src := postgres.NewOutboxSource(pool)

	// Broker failure: no messages are marked published.
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

// Two concurrent relays never select the same message (FOR UPDATE SKIP LOCKED).
func TestOutboxSource_ConcurrentRelaysDoNotShareMessages(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
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
			<-release // Hold the lock on the first message.
			return allErr(msgs, nil)
		})
		first <- err
	}()
	<-entered
	// Use a timeout: without SKIP LOCKED, the second relay would wait instead of taking the next message.
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
	repo := postgres.NewUserRepository(pool, true)
	if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); err != nil {
		t.Fatal(err)
	}
	src := postgres.NewOutboxSource(pool)
	if _, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error { return allErr(msgs, nil) }); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, newUser(t, userB, "b@example.com")); err != nil { // Not yet published.
		t.Fatal(err)
	}
	if n, err := src.Purge(ctx, time.Now().Add(-time.Hour), 1000); err != nil || n != 0 {
		t.Fatalf("purge before publish time: n=%d err=%v, want 0", n, err)
	}
	if n, err := src.Purge(ctx, time.Now().Add(time.Second), 1000); err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v, want 1", n, err)
	}
	if rows := outboxRows(t, pool); len(rows) != 1 || rows[0].AggregateID != userB {
		t.Fatalf("remaining rows %+v, want only the unpublished one", rows)
	}
}

func ptr(s string) *string { return &s }

// allErr returns the same result for all messages (nil = the broker accepted all of them).
func allErr(msgs []outbox.Message, err error) []error {
	errs := make([]error, len(msgs))
	for i := range errs {
		errs[i] = err
	}
	return errs
}

// Per-message results: mark successes, park permanent rejections (never selected again or purged),
// and leave temporary failures in the queue.
func TestOutboxSource_MarksEachMessageByItsOwnResult(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
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

	// Only the temporary failure remains next time; parked messages do not return.
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
	if n, err := src.Purge(ctx, time.Now().Add(time.Hour), 1000); err != nil || n != 2 {
		t.Fatalf("purge = %d, %v; want the 2 published rows, not the parked one", n, err)
	}
}

// pauseQuery holds the first query containing match on the client until the test releases it.
// Contains, not HasPrefix: generated queries start with a "-- name: ..." comment.
type pauseQuery struct {
	match            string
	reached, release chan struct{}
}

func newPause(match string) *pauseQuery {
	return &pauseQuery{match: match, reached: make(chan struct{}), release: make(chan struct{})}
}

func (p *pauseQuery) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, p.match) {
		close(p.reached)
		<-p.release
	}
	return ctx
}

func (*pauseQuery) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// pausedPool returns a pool on dsn whose queries go through tracer.
func pausedPool(t *testing.T, dsn string, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// A user's events must follow commit order even when Delete starts before an Update that
// commits first (the relay sorts by outbox insertion time).
func TestOutbox_EventOrderFollowsCommitOrder(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	// Hold DELETE: the transaction has begun but has not locked the row, allowing another Update
	// to commit first.
	pause := newPause("DELETE FROM users")
	pool := pausedPool(t, dsn, pause)
	repo := postgres.NewUserRepository(pool, true)

	u := newUser(t, userA, "order@example.com")
	if err := repo.Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- repo.Delete(ctx, u.ID) }()
	<-pause.reached // Delete has begun but has not executed DELETE.
	next, err := u.Apply(domain.Changes{Name: ptr("Renamed")}, u.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.Update(ctx, next, u.Version); err != nil { // Commit before Delete.
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

// Service operations that do not change data emit no event: unchanged PATCH values,
// If-Match version mismatch (412), invalid input (422), and duplicate email (409).
func TestOutbox_NoEventWhenNothingChanges(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
	svc := service.NewUserService(repo, system.UUIDv7{}, system.Clock{}, nil)
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

// Update and Delete are atomic with their events too (Create is covered by TestOutbox_IsAtomicWithTheWrite).
func TestOutbox_UpdateAndDeleteAreAtomicWithTheirEvents(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
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

// Each write and its event run on one transaction connection: until it commits, other
// connections see neither the data change nor the event. (Dropping the outbox table, as the
// atomicity tests do, cannot detect an event inserted outside the transaction.)
func TestOutbox_WriteAndEventShareOneTransaction(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t)
	ctx := context.Background()
	if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	pause := &pauseQuery{match: "commit"}
	repo := postgres.NewUserRepository(pausedPool(t, dsn, pause), true)
	observer, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(observer.Close)
	seen := postgres.NewUserRepository(observer, false)

	u := newUser(t, userA, "a@example.com")
	next, err := u.Apply(domain.Changes{Name: ptr("Renamed")}, u.UpdatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for i, step := range []struct {
		name    string
		write   func() error
		visible func() bool // Whether other connections see the data change.
	}{
		{"create", func() error { return repo.Create(ctx, u) }, func() bool {
			_, err := seen.GetByID(ctx, u.ID)
			return err == nil
		}},
		{"update", func() error { return repo.Update(ctx, next, u.Version) }, func() bool {
			got, err := seen.GetByID(ctx, u.ID)
			return err == nil && got.Version == next.Version
		}},
		{"delete", func() error { return repo.Delete(ctx, u.ID) }, func() bool {
			_, err := seen.GetByID(ctx, u.ID)
			return errors.Is(err, port.ErrNotFound)
		}},
	} {
		pause.reached, pause.release = make(chan struct{}), make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- step.write() }()
		select { // The write and its event are done; COMMIT has not been sent.
		case <-pause.reached:
		case err := <-done: // Without a transaction, no COMMIT is ever sent.
			t.Fatalf("%s finished without committing a transaction: %v", step.name, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("%s never reached COMMIT", step.name)
		}
		changed, events := step.visible(), len(outboxRows(t, observer))
		close(pause.release)
		if err := <-done; err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if changed || events != i {
			t.Errorf("%s before commit: change visible=%v, %d events visible; want neither", step.name, changed, events-i)
		}
		if !step.visible() || len(outboxRows(t, observer)) != i+1 {
			t.Errorf("%s after commit: want the change and exactly one new event", step.name)
		}
	}
}

// A canceled request context stops reads and writes and keeps its identity through mapError.
func TestUserRepository_CanceledContext(t *testing.T) {
	pool := freshPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, outboxOn := range []bool{false, true} {
		repo := postgres.NewUserRepository(pool, outboxOn)
		if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); !errors.Is(err, context.Canceled) {
			t.Errorf("Create (outbox %v) = %v, want context.Canceled", outboxOn, err)
		}
		if _, err := repo.GetByID(ctx, userA); !errors.Is(err, context.Canceled) {
			t.Errorf("GetByID = %v, want context.Canceled", err)
		}
	}
	if _, err := postgres.NewUserRepository(pool, true).GetByID(context.Background(), userA); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("user written despite cancellation: %v", err)
	}
}

// publishUsers creates n users and publishes their outbox rows.
func publishUsers(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	ctx := context.Background()
	repo := postgres.NewUserRepository(pool, true)
	for i := range n {
		id := fmt.Sprintf("01a15359-aa28-7a00-a380-c47e529061%02d", i)
		if err := repo.Create(ctx, newUser(t, id, fmt.Sprintf("p%d@example.com", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := postgres.NewOutboxSource(pool).Process(ctx, 100, func(_ context.Context, msgs []outbox.Message) []error {
		return allErr(msgs, nil)
	}); err != nil {
		t.Fatal(err)
	}
}

func countOutbox(t *testing.T, pool *pgxpool.Pool, where string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE `+where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Purge deletes at most limit published rows per call; pending and parked rows always remain.
func TestOutboxSource_PurgeDeletesAtMostLimit(t *testing.T) {
	pool := freshPool(t)
	ctx := context.Background()
	publishUsers(t, pool, 3)
	repo := postgres.NewUserRepository(pool, true)
	if err := repo.Create(ctx, newUser(t, userA, "a@example.com")); err != nil { // Parked below.
		t.Fatal(err)
	}
	src := postgres.NewOutboxSource(pool)
	if _, err := src.Process(ctx, 10, func(_ context.Context, msgs []outbox.Message) []error {
		return allErr(msgs, fmt.Errorf("MESSAGE_TOO_LARGE: %w", outbox.ErrPermanent))
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, newUser(t, userB, "b@example.com")); err != nil { // Stays pending.
		t.Fatal(err)
	}
	for _, want := range []int64{2, 1, 0} {
		if n, err := src.Purge(ctx, time.Now().Add(time.Second), 2); err != nil || n != want {
			t.Fatalf("purge = %d, %v; want %d", n, err, want)
		}
	}
	if parked, pending := countOutbox(t, pool, "parked_at IS NOT NULL"), countOutbox(t, pool, "published_at IS NULL AND parked_at IS NULL"); parked != 1 || pending != 1 {
		t.Fatalf("parked=%d pending=%d, want 1 and 1", parked, pending)
	}
}

// A chunk that exceeds statement_timeout rolls back only itself: earlier chunks stay deleted.
func TestOutboxSource_PurgeChunkTimeoutKeepsEarlierChunks(t *testing.T) {
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
	publishUsers(t, pool, 4)
	if n, err := postgres.NewOutboxSource(pool).Purge(ctx, time.Now().Add(time.Second), 2); err != nil || n != 2 {
		t.Fatalf("first chunk = %d, %v; want 2", n, err)
	}
	if _, err := pool.Exec(ctx, `CREATE FUNCTION slow_delete() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN PERFORM pg_sleep(0.1); RETURN OLD; END$$`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER slow_delete BEFORE DELETE ON outbox FOR EACH ROW EXECUTE FUNCTION slow_delete()`); err != nil {
		t.Fatal(err)
	}
	slow, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 1, StatementTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(slow.Close)
	if _, err := postgres.NewOutboxSource(slow).Purge(ctx, time.Now().Add(time.Second), 2); err == nil || !strings.Contains(err.Error(), "57014") {
		t.Fatalf("slow chunk error = %v, want statement timeout (57014)", err)
	}
	if n := countOutbox(t, pool, "published_at IS NOT NULL"); n != 2 {
		t.Fatalf("published rows = %d, want the 2 the failed chunk rolled back", n)
	}
}

// Parked rows are never purged and are the oldest unpublished rows: the pending index excludes
// them so the relay's pending query does not walk past them on every poll.
func TestMigrations_PendingIndexExcludesParkedRows(t *testing.T) {
	pool := freshPool(t)
	rows, err := pool.Query(context.Background(), `SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'outbox' AND schemaname = current_schema()`)
	if err != nil {
		t.Fatal(err)
	}
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
		var d [2]string
		return d, r.Scan(&d[0], &d[1])
	})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]string{}
	for _, d := range defs {
		byName[d[0]] = d[1]
	}
	def, ok := byName["outbox_pending_idx"]
	if !ok || !strings.Contains(def, "published_at IS NULL") || !strings.Contains(def, "parked_at IS NULL") {
		t.Fatalf("outbox_pending_idx = %q (exists %v), want a partial index excluding published and parked rows", def, ok)
	}
	if _, ok := byName["outbox_unpublished_idx"]; ok {
		t.Fatal("outbox_unpublished_idx still exists")
	}
}
