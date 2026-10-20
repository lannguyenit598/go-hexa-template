package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/example/userapi/internal/outbox"
)

// memSource là Source in-memory: Process chỉ đánh dấu đã publish khi publish thành công,
// giống transaction của adapter postgres (lỗi => rollback, message còn nguyên).
type memSource struct {
	mu        sync.Mutex
	pending   []outbox.Message
	published []outbox.Message
	purges    []time.Time
}

func (s *memSource) Process(ctx context.Context, limit int, publish func(context.Context, []outbox.Message) error) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.pending[:min(limit, len(s.pending))]
	if len(batch) == 0 {
		return 0, nil
	}
	if err := publish(ctx, batch); err != nil {
		return 0, err
	}
	s.published = append(s.published, batch...)
	s.pending = s.pending[len(batch):]
	return len(batch), nil
}

func (s *memSource) Purge(_ context.Context, before time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purges = append(s.purges, before)
	return 0, nil
}

func (s *memSource) snapshot() (pending, published int, purges []time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending), len(s.published), append([]time.Time(nil), s.purges...)
}

type sinkFunc func(context.Context, []outbox.Message) error

func (f sinkFunc) Publish(ctx context.Context, msgs []outbox.Message) error { return f(ctx, msgs) }

func messages(n int) []outbox.Message {
	out := make([]outbox.Message, n)
	for i := range out {
		out[i] = outbox.Message{ID: fmt.Sprintf("m%d", i), Key: "u1", Type: "user.updated", Payload: []byte(`{}`)}
	}
	return out
}

func newRelay(src outbox.Source, sink outbox.Sink) *outbox.Relay {
	return &outbox.Relay{
		Source: src, Sink: sink, Logger: slog.New(slog.DiscardHandler),
		BatchSize: 2, PollInterval: 10 * time.Millisecond, Retention: time.Hour,
	}
}

// startRelay chạy relay trong goroutine; trả hàm huỷ ctx và hàm chờ Run trả về.
func startRelay(r *outbox.Relay) (cancel context.CancelFunc, wait func() error) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return cancel, func() error {
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			return errors.New("Run did not return after cancel")
		}
	}
}

func runRelay(r *outbox.Relay) (stop func() error) {
	cancel, wait := startRelay(r)
	return func() error { cancel(); return wait() }
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRelay_PublishesEverythingInOrder(t *testing.T) {
	src := &memSource{pending: messages(5)}
	var mu sync.Mutex
	var got []string
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) error {
		mu.Lock()
		defer mu.Unlock()
		if len(msgs) > 2 {
			return fmt.Errorf("batch of %d exceeds BatchSize", len(msgs))
		}
		for _, m := range msgs {
			got = append(got, m.ID)
		}
		return nil
	})))
	waitFor(t, func() bool { _, published, _ := src.snapshot(); return published == 5 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != "[m0 m1 m2 m3 m4]" {
		t.Fatalf("published %v", got)
	}
}

// Broker lỗi: message không mất, relay thử lại ở lần poll sau.
func TestRelay_RetriesAfterSinkFailure(t *testing.T) {
	src := &memSource{pending: messages(1)}
	var mu sync.Mutex
	failures := 0
	stop := runRelay(newRelay(src, sinkFunc(func(context.Context, []outbox.Message) error {
		mu.Lock()
		defer mu.Unlock()
		if failures < 2 {
			failures++
			return errors.New("broker unavailable")
		}
		return nil
	})))
	waitFor(t, func() bool { _, published, _ := src.snapshot(); return published == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// Tắt relay giữa lúc đang publish: lô đó vẫn được publish xong và đánh dấu, để lần chạy sau
// không gửi trùng.
func TestRelay_FinishesInFlightBatchOnShutdown(t *testing.T) {
	src := &memSource{pending: messages(1)}
	entered, release := make(chan struct{}), make(chan struct{})
	cancel, wait := startRelay(newRelay(src, sinkFunc(func(ctx context.Context, _ []outbox.Message) error {
		close(entered)
		<-release
		return ctx.Err() // lỗi nếu ctx của lô bị huỷ theo ctx của Run
	})))
	<-entered
	cancel() // tín hiệu tắt tới trong lúc lô đang publish
	close(release)
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if pending, published, _ := src.snapshot(); pending != 0 || published != 1 {
		t.Fatalf("pending=%d published=%d, want the in-flight batch published", pending, published)
	}
}

func TestRelay_PurgesPublishedMessagesOlderThanRetention(t *testing.T) {
	src := &memSource{}
	start := time.Now()
	stop := runRelay(newRelay(src, sinkFunc(func(context.Context, []outbox.Message) error { return nil })))
	waitFor(t, func() bool { _, _, purges := src.snapshot(); return len(purges) > 0 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_, _, purges := src.snapshot()
	if want := start.Add(-time.Hour); purges[0].Before(want.Add(-time.Second)) || purges[0].After(time.Now().Add(-time.Hour)) {
		t.Fatalf("purge before %v, want about now - Retention (%v)", purges[0], want)
	}
}
