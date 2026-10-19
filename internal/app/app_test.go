package app_test

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

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
