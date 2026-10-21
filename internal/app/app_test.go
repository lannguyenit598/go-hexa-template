package app_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/example/userapi/internal/adapter/outbound/postgres/pgtest"
	"github.com/example/userapi/internal/app"
	"github.com/example/userapi/internal/config"
)

// Disable client keep-alive: reusing an existing connection would hide a closed listener.
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

// startApp listens on 127.0.0.1:<random port> and waits up to readyWithin for /healthz to return 200.
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
	expectGET(t, base+"/readyz", http.StatusServiceUnavailable) // The load balancer stops routing new traffic.
	expectGET(t, base+"/api/v1/users", http.StatusOK)           // Existing traffic is still served.
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

// A new Storage constant without a storageModule branch must fail startup,
// not silently fall back to in-memory storage.
func TestRun_RejectsUnknownStorage(t *testing.T) {
	cfg := mustConfig(t, nil)
	cfg.Storage = "mysql"
	err := runWithTimeout(t, context.Background(), cfg, app.Options{Listener: listen(t)}, 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), `unsupported storage "mysql"`) {
		t.Fatalf("Run = %v, want unsupported storage error", err)
	}
}

// Cancel requests still running after SHUTDOWN_TIMEOUT so Run returns rather than waiting for
// pool.Close() to wait for queries; the process must not exceed its shutdown budget.
func TestRun_CancelsInFlightRequestsWhenDrainTimesOut(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
	base, cancel, done := startApp(t, map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true",
		"SHUTDOWN_TIMEOUT": "200ms", "REQUEST_TIMEOUT": "30s", "DB_STATEMENT_TIMEOUT": "30s",
	}, 15*time.Second)

	// Lock users so GET /api/v1/users blocks.
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

// waitForLockWaiter waits for a query waiting on a lock (the request has reached the database).
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
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
	env := map[string]string{"STORAGE": "postgres", "DATABASE_URL": dsn}
	ln := listen(t)
	err := runWithTimeout(t, context.Background(), mustConfig(t, env), app.Options{Listener: ln}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "pending migrations") {
		t.Fatalf("Run = %v, want pending migrations error", err)
	}

	env["MIGRATE_ON_START"] = "true"
	// The goose advisory lock is database-wide. Concurrent package migrations (go test ./...)
	// retry lock acquisition every 5s, so allow longer than one retry interval.
	base, cancel, done := startApp(t, env, 15*time.Second)
	expectGET(t, base+"/api/v1/users", http.StatusOK)
	cancel()
	if err := waitDone(t, done, 3*time.Second); err != nil {
		t.Fatal(err)
	}
}

// Full event flow: the API writes user + outbox atomically; the relay reads the outbox
// and publishes to an in-memory kfake Kafka cluster, using the user ID as the key.
func TestRelay_DeliversEventsWrittenByTheAPI(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
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

// Adding an EventsSink constant without a sinkModule branch must fail relay startup before
// database connection.
func TestRunRelay_RejectsUnknownSink(t *testing.T) {
	cfg := mustConfig(t, nil)
	cfg.Events.Sink = "rabbitmq"
	err := app.RunRelay(context.Background(), cfg, quiet, app.Options{})
	if err == nil || !strings.Contains(err.Error(), `unsupported events sink "rabbitmq"`) {
		t.Fatalf("RunRelay = %v, want unsupported events sink", err)
	}
}

// KAFKA_SASL_* settings must reach the Kafka client: valid credentials pass broker connection
// and fail only at the intentionally unreachable database; invalid credentials fail at the broker.
func TestRunRelay_PassesSASLCredentialsToKafka(t *testing.T) {
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"),
		kfake.EnableSASL(), kfake.Superuser("SCRAM-SHA-256", "relay", "s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	for _, tc := range []struct{ password, wantErr string }{
		{"s3cret", "ping database"},
		{"wrong", "kafka ping"},
	} {
		cfg := mustConfig(t, map[string]string{
			"STORAGE": "postgres", "DATABASE_URL": "postgres://127.0.0.1:1/db?connect_timeout=1",
			"EVENTS_SINK": "kafka", "KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
			"KAFKA_SASL_MECHANISM": "SCRAM-SHA-256", "KAFKA_SASL_USERNAME": "relay", "KAFKA_SASL_PASSWORD": tc.password,
		})
		err := app.RunRelay(context.Background(), cfg, quiet, app.Options{})
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Fatalf("password %q: RunRelay = %v, want error containing %q", tc.password, err, tc.wantErr)
		}
	}
}

func TestRunRelay_ServesHealthWhileRunning(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	cfg := mustConfig(t, map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true",
		"EVENTS_SINK": "kafka", "KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
	})
	health := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- app.RunRelay(ctx, cfg, quiet, app.Options{Listener: health}) }()

	url := "http://" + health.Addr().String() + "/healthz"
	deadline := time.Now().Add(15 * time.Second) // MIGRATE_ON_START may wait for the goose advisory lock.
	for {
		if resp, err := get(url); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("relay health endpoint never returned 200")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := waitDone(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if resp, err := get(url); err == nil {
		_ = resp.Body.Close()
		t.Fatal("health endpoint still open after the relay stopped")
	}
}

// hookRecorder captures, in order, the constructors whose lifecycle hooks fx runs
// (fx logs them at debug level through the injected logger).
type hookRecorder struct {
	mu          sync.Mutex
	start, stop []string
}

func (h *hookRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *hookRecorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *hookRecorder) WithGroup(string) slog.Handler            { return h }

func (h *hookRecorder) Handle(_ context.Context, r slog.Record) error {
	var caller string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "caller" {
			caller = a.Value.String()[strings.LastIndex(a.Value.String(), ".")+1:]
		}
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	switch r.Message {
	case "OnStart hook executing":
		h.start = append(h.start, caller)
	case "OnStop hook executing":
		h.stop = append(h.stop, caller)
	}
	return nil
}

// runRecorded runs until healthURL returns 200, stops, and returns the hook order.
func runRecorded(t *testing.T, healthURL string, run func(context.Context, *slog.Logger) error) (start, stop []string) {
	t.Helper()
	rec := &hookRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- run(ctx, slog.New(rec)) }()
	deadline := time.Now().Add(15 * time.Second) // MIGRATE_ON_START may wait for the goose advisory lock.
	for {
		if resp, err := get(healthURL); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("never became healthy")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := waitDone(t, done, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	return rec.start, rec.stop
}

func expectOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s hooks = %v, want %v", what, got, want)
	}
}

// Telemetry starts first and flushes last; the HTTP server drains before the pool closes.
func TestRun_HookOrder(t *testing.T) {
	ln := listen(t)
	start, stop := runRecorded(t, "http://"+ln.Addr().String()+"/healthz", func(ctx context.Context, l *slog.Logger) error {
		return app.Run(ctx, mustConfig(t, nil), l, app.Options{Listener: ln})
	})
	expectOrder(t, "start", start, []string{"startTelemetry", "newHTTPServer"})
	expectOrder(t, "stop", stop, []string{"newHTTPServer", "startTelemetry"})

	dsn := pgtest.FreshSchemaDSN(t) // Skip the postgres variant without TEST_DATABASE_URL.
	ln = listen(t)
	cfg := mustConfig(t, map[string]string{"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true"})
	start, stop = runRecorded(t, "http://"+ln.Addr().String()+"/healthz", func(ctx context.Context, l *slog.Logger) error {
		return app.Run(ctx, cfg, l, app.Options{Listener: ln})
	})
	expectOrder(t, "start", start, []string{"startTelemetry", "openPool", "newHTTPServer"})
	expectOrder(t, "stop", stop, []string{"newHTTPServer", "openPool", "startTelemetry"})
}

// The broker is checked before the database; the relay loop stops before the pool and sink close.
func TestRunRelay_HookOrder(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
	cluster, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "users"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cluster.Close)
	cfg := mustConfig(t, map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true",
		"EVENTS_SINK": "kafka", "KAFKA_BROKERS": strings.Join(cluster.ListenAddrs(), ","),
	})
	health := listen(t)
	start, stop := runRecorded(t, "http://"+health.Addr().String()+"/healthz", func(ctx context.Context, l *slog.Logger) error {
		return app.RunRelay(ctx, cfg, l, app.Options{Listener: health})
	})
	expectOrder(t, "start", start, []string{"startTelemetry", "newKafkaSink", "openPool", "newRelay", "serveRelayHealth"})
	expectOrder(t, "stop", stop, []string{"serveRelayHealth", "newRelay", "openPool", "newKafkaSink", "startTelemetry"})
}

// A signal during startup must abort steps waiting on the database (ping, goose lock retries),
// or the orchestrator kills the pod after its grace period instead of letting it exit.
func TestRun_SignalAbortsStartupWaitingOnDatabase(t *testing.T) {
	// A server that accepts connections and never answers: the database ping hangs.
	silent := listen(t)
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	cfg := mustConfig(t, map[string]string{
		"STORAGE": "postgres", "DATABASE_URL": "postgres://u:p@" + silent.Addr().String() + "/db?sslmode=disable",
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	if err := runWithTimeout(t, ctx, cfg, app.Options{Listener: listen(t)}, 2*time.Second); err == nil {
		t.Fatal("Run = nil, want startup error after cancellation")
	}
}

func TestRun_ServesAPIDocsOnlyWhenEnabled(t *testing.T) {
	base, _, _ := startApp(t, nil, 5*time.Second)
	expectGET(t, base+"/openapi.json", http.StatusNotFound)
	expectGET(t, base+"/docs", http.StatusNotFound)

	base, _, _ = startApp(t, map[string]string{"API_DOCS_ENABLED": "true"}, 5*time.Second)
	expectGET(t, base+"/openapi.json", http.StatusOK)
	expectGET(t, base+"/docs", http.StatusOK)
}

// send issues a JSON request and returns the status and Location header.
func send(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// PostgreSQL TEXT cannot store U+0000: with postgres storage a NUL name is a 422, as in memory, not a 500.
func TestRun_PostgresRejectsNULInName(t *testing.T) {
	dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
	base, _, _ := startApp(t, map[string]string{"STORAGE": "postgres", "DATABASE_URL": dsn, "MIGRATE_ON_START": "true"}, 15*time.Second)
	if code, _ := send(t, http.MethodPost, base+"/api/v1/users", `{"email":"nul@example.com","name":"a\u0000b"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("POST NUL name = %d, want 422", code)
	}
	code, loc := send(t, http.MethodPost, base+"/api/v1/users", `{"email":"ok@example.com","name":"Ok"}`)
	if code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201", code)
	}
	if code, _ := send(t, http.MethodPatch, base+loc, `{"name":"a\u0000b"}`); code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH NUL name = %d, want 422", code)
	}
}
