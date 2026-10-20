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

// memSource là Source in-memory với cùng ngữ nghĩa adapter postgres: message publish thành công
// được đánh dấu, message bị từ chối vĩnh viễn (ErrPermanent) bị tách riêng, message lỗi tạm thời
// ở lại hàng đợi theo đúng thứ tự cũ.
type memSource struct {
	mu        sync.Mutex
	pending   []outbox.Message
	published []outbox.Message
	parked    []outbox.Message
	purges    []time.Time
}

func (s *memSource) Process(ctx context.Context, limit int, publish func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.pending[:min(limit, len(s.pending))]
	if len(batch) == 0 {
		return outbox.Result{}, nil
	}
	errs := publish(ctx, batch)
	var res outbox.Result
	var keep []outbox.Message
	for i, m := range batch {
		switch err := errs[i]; {
		case err == nil:
			s.published = append(s.published, m)
			res.Published++
		case errors.Is(err, outbox.ErrPermanent):
			s.parked = append(s.parked, m)
			res.Parked = append(res.Parked, m.ID)
		default:
			keep = append(keep, m)
			res.Retry++
		}
		if errs[i] != nil && res.Err == nil {
			res.Err = errs[i]
		}
	}
	s.pending = append(keep, s.pending[len(batch):]...)
	return res, nil
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

type sinkFunc func(context.Context, []outbox.Message) []error

func (f sinkFunc) Publish(ctx context.Context, msgs []outbox.Message) []error { return f(ctx, msgs) }

// each trả cùng một lỗi cho mọi message (nil = tất cả thành công).
func each(msgs []outbox.Message, err error) []error {
	errs := make([]error, len(msgs))
	for i := range errs {
		errs[i] = err
	}
	return errs
}

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
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		mu.Lock()
		defer mu.Unlock()
		if len(msgs) > 2 {
			return each(msgs, fmt.Errorf("batch of %d exceeds BatchSize", len(msgs)))
		}
		for _, m := range msgs {
			got = append(got, m.ID)
		}
		return each(msgs, nil)
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
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		mu.Lock()
		defer mu.Unlock()
		if failures < 2 {
			failures++
			return each(msgs, errors.New("broker unavailable"))
		}
		return each(msgs, nil)
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
	cancel, wait := startRelay(newRelay(src, sinkFunc(func(ctx context.Context, msgs []outbox.Message) []error {
		close(entered)
		<-release
		return each(msgs, ctx.Err()) // lỗi nếu ctx của lô bị huỷ theo ctx của Run
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
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error { return each(msgs, nil) })))
	waitFor(t, func() bool { _, _, purges := src.snapshot(); return len(purges) > 0 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	_, _, purges := src.snapshot()
	if want := start.Add(-time.Hour); purges[0].Before(want.Add(-time.Second)) || purges[0].After(time.Now().Add(-time.Hour)) {
		t.Fatalf("purge before %v, want about now - Retention (%v)", purges[0], want)
	}
}

// Một message bị broker từ chối vĩnh viễn (ví dụ quá lớn) bị tách riêng: nó không chặn các message
// phía sau, và các message đã gửi thành công trong cùng lô không bị gửi lại.
func TestRelay_PermanentFailureIsParkedAndDoesNotBlockOthers(t *testing.T) {
	src := &memSource{pending: messages(5)}
	var mu sync.Mutex
	deliveries := map[string]int{}
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		mu.Lock()
		defer mu.Unlock()
		errs := make([]error, len(msgs))
		for i, m := range msgs {
			if m.ID == "m1" {
				errs[i] = fmt.Errorf("message too large: %w", outbox.ErrPermanent)
				continue
			}
			deliveries[m.ID]++
		}
		return errs
	})))
	waitFor(t, func() bool { pending, published, _ := src.snapshot(); return pending == 0 && published == 4 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for id, n := range deliveries {
		if n != 1 {
			t.Fatalf("%s delivered %d times, want once", id, n)
		}
	}
	if len(src.parked) != 1 || src.parked[0].ID != "m1" {
		t.Fatalf("parked %v, want [m1]", src.parked)
	}
}

// Lỗi tạm thời chỉ của một message: message đó được thử lại, message đã thành công không bị gửi lại.
func TestRelay_PartialTransientFailureOnlyRetriesTheFailedMessage(t *testing.T) {
	src := &memSource{pending: messages(2)}
	var mu sync.Mutex
	deliveries := map[string]int{}
	failedOnce := false
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		mu.Lock()
		defer mu.Unlock()
		errs := make([]error, len(msgs))
		for i, m := range msgs {
			if m.ID == "m1" && !failedOnce {
				failedOnce = true
				errs[i] = errors.New("NOT_LEADER_FOR_PARTITION")
				continue
			}
			deliveries[m.ID]++
		}
		return errs
	})))
	waitFor(t, func() bool { _, published, _ := src.snapshot(); return published == 2 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deliveries["m0"] != 1 || deliveries["m1"] != 1 {
		t.Fatalf("deliveries %v, want each message exactly once", deliveries)
	}
}

// Liveness: heartbeat tiến lên khi vòng lặp chạy; khi một lô bị treo, heartbeat đứng yên và relay
// được coi là không khoẻ sau ngưỡng HealthThreshold (để Kubernetes khởi động lại pod).
func TestRelay_HealthFollowsLoopProgress(t *testing.T) {
	src := &memSource{pending: messages(1)}
	entered, release := make(chan struct{}), make(chan struct{})
	r := newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		close(entered)
		<-release // lô đầu tiên bị treo tới khi test cho chạy tiếp
		return each(msgs, nil)
	}))
	if r.Healthy(time.Now()) {
		t.Fatal("relay reported healthy before it started")
	}
	cancel, wait := startRelay(r)
	defer func() { cancel(); _ = wait() }()
	<-entered

	stuckSince := r.LastActivity()
	if !r.Healthy(stuckSince.Add(r.HealthThreshold() - time.Second)) {
		t.Fatal("relay unhealthy before the threshold")
	}
	if r.Healthy(stuckSince.Add(r.HealthThreshold() + time.Second)) {
		t.Fatal("relay still healthy after being stuck longer than the threshold")
	}

	close(release)
	waitFor(t, func() bool { return r.LastActivity().After(stuckSince) })
}
