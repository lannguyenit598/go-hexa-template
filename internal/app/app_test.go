package app_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/example/userapi/internal/adapter/outbound/postgres/pgtest"
	"github.com/example/userapi/internal/app"
	"github.com/example/userapi/internal/config"
)

// client không giữ keep-alive: dùng lại connection cũ sẽ che mất việc listener đã đóng.
var client = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
	Timeout:   time.Second,
}

var quiet = slog.New(slog.DiscardHandler)

func get(url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func mustConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()
	cfg, err := config.Load(func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func expectGET(t *testing.T, url string, want int) {
	t.Helper()
	resp, err := get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, want)
	}
}

func waitDone(t *testing.T, done <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("Run did not return")
		return nil
	}
}

func runWithTimeout(t *testing.T, ctx context.Context, cfg config.Config, opts app.Options, timeout time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx, cfg, quiet, opts) }()
	return waitDone(t, done, timeout)
}

// startApp chạy app trên 127.0.0.1:<cổng ngẫu nhiên> và chờ tối đa readyWithin tới khi /healthz trả 200.
func startApp(t *testing.T, env map[string]string, readyWithin time.Duration) (base string, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	ln := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch := make(chan error, 1)
	go func() { ch <- app.Run(ctx, mustConfig(t, env), quiet, app.Options{Listener: ln}) }()

	base = "http://" + ln.Addr().String()
	deadline := time.Now().Add(readyWithin)
	for {
		if resp, err := get(base + "/healthz"); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base, cancel, ch
			}
		}
		select {
		case err := <-ch:
			t.Fatalf("Run returned before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("app did not become healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRun_DrainsBeforeClosingListener(t *testing.T) {
	base, cancel, done := startApp(t, map[string]string{"SHUTDOWN_DELAY": "500ms"}, 2*time.Second)
	expectGET(t, base+"/readyz", http.StatusOK)
	cancel()
	time.Sleep(100 * time.Millisecond)
	expectGET(t, base+"/readyz", http.StatusServiceUnavailable) // LB ngừng gửi traffic mới
	expectGET(t, base+"/api/v1/users", http.StatusOK)           // nhưng vẫn phục vụ
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if resp, err := get(base + "/healthz"); err == nil {
		_ = resp.Body.Close()
		t.Fatal("listener still open after shutdown")
	}
}

func TestRun_FailsFastWhenAddressInUse(t *testing.T) {
	busy := listen(t)
	t.Cleanup(func() { _ = busy.Close() })
	cfg := mustConfig(t, map[string]string{"HTTP_ADDR": busy.Addr().String()})
	err := runWithTimeout(t, context.Background(), cfg, app.Options{}, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("Run = %v, want listen error", err)
	}
}

func TestRun_CanceledBeforeStartReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ln := listen(t)
	if err := runWithTimeout(t, ctx, mustConfig(t, nil), app.Options{Listener: ln}, time.Second); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

// Thêm hằng Storage mới mà quên nhánh trong newRepository thì phải lỗi khi khởi động,
// không được âm thầm chạy bằng repository in-memory.
func TestRun_RejectsUnknownStorage(t *testing.T) {
	cfg := mustConfig(t, nil)
	cfg.Storage = "mysql"
	err := runWithTimeout(t, context.Background(), cfg, app.Options{Listener: listen(t)}, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), `unsupported storage "mysql"`) {
		t.Fatalf("Run = %v, want unsupported storage error", err)
	}
}

// Request vẫn chạy khi hết SHUTDOWN_TIMEOUT bị huỷ, để Run trả về ngay thay vì chờ
// pool.Close() đợi query xong; tiến trình không được sống quá ngân sách đã khai báo.
func TestRun_CancelsInFlightRequestsWhenDrainTimesOut(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // skip nếu không có TEST_DATABASE_URL
	base, cancel, done := startApp(t, map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true",
		"SHUTDOWN_TIMEOUT": "200ms", "REQUEST_TIMEOUT": "30s", "DB_STATEMENT_TIMEOUT": "30s",
	}, 15*time.Second)

	// Khoá bảng users để request GET /api/v1/users bị treo.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	slowClient := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/users", nil)
	if err != nil {
		t.Fatal(err)
	}
	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		if resp, err := slowClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}()
	waitForLockWaiter(t, conn)

	cancel()
	start := time.Now()
	err = waitDone(t, done, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "graceful shutdown") {
		t.Fatalf("Run = %v, want graceful shutdown error", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Run took %v after the drain deadline", elapsed)
	}
	<-inFlight
}

// waitForLockWaiter chờ tới khi có một truy vấn đang đợi khoá (request đã tới database).
func waitForLockWaiter(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		err := conn.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND application_name = 'userapi'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("request never reached the database")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRun_RefusesToStartWithPendingMigrations(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // skip nếu không có TEST_DATABASE_URL
	env := map[string]string{"STORAGE": "postgres", "DATABASE_URL": dsn}
	ln := listen(t)
	err := runWithTimeout(t, context.Background(), mustConfig(t, env), app.Options{Listener: ln}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "pending migrations") {
		t.Fatalf("Run = %v, want pending migrations error", err)
	}

	env["MIGRATE_ON_START"] = "true"
	// Advisory lock của goose dùng chung cho cả database: khi package khác (go test ./...) đang
	// migrate cùng lúc, goose chờ lock và thử lại mỗi 5s, nên phải chờ lâu hơn một chu kỳ đó.
	base, cancel, done := startApp(t, env, 15*time.Second)
	expectGET(t, base+"/api/v1/users", http.StatusOK)
	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

// Đường đi đầy đủ của một sự kiện: API ghi user + outbox trong một transaction, relay đọc
// outbox và gửi lên Kafka (cluster in-memory kfake), key là ID của user.
func TestRelay_DeliversEventsWrittenByTheAPI(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // skip nếu không có TEST_DATABASE_URL
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	env := map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true",
		"EVENTS_SINK": "kafka", "KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
		"OUTBOX_POLL_INTERVAL": "50ms",
	}
	base, _, _ := startApp(t, env, 15*time.Second)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/api/v1/users",
		strings.NewReader(`{"email":"relay@example.com","name":"Relay"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	userID := strings.TrimPrefix(resp.Header.Get("Location"), "/api/v1/users/")

	relayCtx, stopRelay := context.WithCancel(context.Background())
	relayDone := make(chan error, 1)
	cfg := mustConfig(t, env)
	go func() { relayDone <- app.RunRelay(relayCtx, cfg, quiet, app.Options{}) }()

	consumer, err := kgo.NewClient(kgo.SeedBrokers(cluster.ListenAddrs()...), kgo.ConsumeTopics("users"),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer consumer.Close()
	pollCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var rec *kgo.Record
	for rec == nil {
		fetches := consumer.PollFetches(pollCtx)
		if pollCtx.Err() != nil {
			t.Fatal("no event reached Kafka")
		}
		fetches.EachRecord(func(r *kgo.Record) { rec = r })
	}
	if string(rec.Key) != userID || !strings.Contains(string(rec.Value), `"event_type":"user.registered"`) {
		t.Fatalf("record key %q value %s, want user.registered for %s", rec.Key, rec.Value, userID)
	}

	stopRelay()
	if err := waitDone(t, relayDone, 5*time.Second); err != nil {
		t.Fatalf("RunRelay = %v, want nil after cancel", err)
	}
}

func TestRunRelay_RefusesWhenEventsAreDisabled(t *testing.T) {
	err := app.RunRelay(context.Background(), mustConfig(t, nil), quiet, app.Options{})
	if err == nil || !strings.Contains(err.Error(), "EVENTS_SINK") {
		t.Fatalf("RunRelay = %v, want EVENTS_SINK error", err)
	}
}
