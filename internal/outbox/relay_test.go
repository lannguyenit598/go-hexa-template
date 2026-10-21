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

// memSource implements Source in memory with PostgreSQL semantics: mark published messages,
// park permanent rejections (ErrPermanent), and keep temporary failures
// in their original queue order.
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

func (s *memSource) Purge(_ context.Context, before time.Time, _ int) (int64, error) {
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

// each returns the same error for all messages (nil = all successful).
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

// startRelay starts Run in a goroutine and returns cancellation and completion-wait functions.
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

// Broker failure: messages remain and are retried after the poll interval.
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

// Shutdown during publishing still completes and marks the batch, avoiding duplicates
// on the next run.
func TestRelay_FinishesInFlightBatchOnShutdown(t *testing.T) {
	src := &memSource{pending: messages(1)}
	entered, release := make(chan struct{}), make(chan struct{})
	cancel, wait := startRelay(newRelay(src, sinkFunc(func(ctx context.Context, msgs []outbox.Message) []error {
		close(entered)
		<-release
		return each(msgs, ctx.Err()) // Fails if Run's context cancellation reaches the batch context.
	})))
	<-entered
	cancel() // Shutdown arrives while publishing.
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

// endlessSource always reports a full, successful batch: a backlog that never drains.
type endlessSource struct{ memSource }

func (s *endlessSource) Process(_ context.Context, limit int, _ func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	return outbox.Result{Published: limit}, nil
}

func TestRelay_PurgesWhileBacklogNeverDrains(t *testing.T) {
	src := &endlessSource{}
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error { return each(msgs, nil) })))
	waitFor(t, func() bool { _, _, purges := src.snapshot(); return len(purges) > 0 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

// A permanent broker rejection (for example, an oversized message) is parked without blocking
// later messages or resending successful messages in the same batch.
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

// A single temporary failure is retried without resending successful messages.
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

// Liveness: the heartbeat advances with the loop. A stuck batch stops the heartbeat, making
// the relay unhealthy after HealthThreshold so Kubernetes can restart the pod.
func TestRelay_HealthFollowsLoopProgress(t *testing.T) {
	src := &memSource{pending: messages(1)}
	entered, release := make(chan struct{}), make(chan struct{})
	r := newRelay(src, sinkFunc(func(_ context.Context, msgs []outbox.Message) []error {
		close(entered)
		<-release // Hold the first batch until the test releases it.
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

// purgeFake records the order of Process ("P") and Purge ("G") calls; purge decides each Purge result.
type purgeFake struct {
	mu     sync.Mutex
	events []string
	limits []int
	full   bool // Process reports full batches (a backlog), so Run reads the next batch at once.
	purge  func(ctx context.Context, limit int) (int64, error)
}

func (s *purgeFake) Process(_ context.Context, limit int, _ func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, "P")
	if s.full {
		return outbox.Result{Published: limit}, nil
	}
	return outbox.Result{}, nil
}

func (s *purgeFake) Purge(ctx context.Context, _ time.Time, limit int) (int64, error) {
	s.mu.Lock()
	s.events = append(s.events, "G")
	s.limits = append(s.limits, limit)
	s.mu.Unlock()
	return s.purge(ctx, limit)
}

func (s *purgeFake) snapshot() (events []string, limits []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...), append([]int(nil), s.limits...)
}

func countPurges(events []string) int {
	n := 0
	for _, e := range events {
		if e == "G" {
			n++
		}
	}
	return n
}

// A backlog larger than one turn is purged in chunks across several turns, never more than
// 10 chunks between two batches, so cleanup cannot take over the loop.
func TestRelay_PurgeDrainsBacklogInBoundedTurns(t *testing.T) {
	src := &purgeFake{purge: func(_ context.Context, limit int) (int64, error) { return int64(limit), nil }}
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, m []outbox.Message) []error { return each(m, nil) })))
	waitFor(t, func() bool { events, _ := src.snapshot(); return countPurges(events) > 10 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	events, limits := src.snapshot()
	for _, l := range limits {
		if l != 1000 {
			t.Fatalf("purge limit = %d, want 1000", l)
		}
	}
	runs, run := 0, 0
	for _, e := range events {
		if e == "G" {
			if run == 0 {
				runs++
			}
			if run++; run > 10 {
				t.Fatalf("more than 10 purge chunks between batches: %v", events)
			}
			continue
		}
		run = 0
	}
	if runs < 2 {
		t.Fatalf("purge turns = %d, want a backlog to continue in a later turn: %v", runs, events)
	}
}

// Each purge chunk is bounded by the 5s purge budget, and the loop beats after it.
func TestRelay_PurgeRunsWithinBudgetAndBeats(t *testing.T) {
	var (
		mu       sync.Mutex
		called   time.Time
		deadline time.Time
		hasDL    bool
		before   time.Time
	)
	var r *outbox.Relay
	src := &purgeFake{purge: func(ctx context.Context, _ int) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		if called.IsZero() {
			called, before = time.Now(), r.LastActivity()
			deadline, hasDL = ctx.Deadline()
		}
		return 0, errors.New("canceling statement due to statement timeout")
	}}
	r = newRelay(src, sinkFunc(func(_ context.Context, m []outbox.Message) []error { return each(m, nil) }))
	stop := runRelay(r)
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return !called.IsZero() && r.LastActivity().After(before)
	})
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !hasDL || deadline.After(called.Add(5*time.Second+100*time.Millisecond)) {
		t.Fatalf("purge deadline = %v (set %v), want at most 5s after %v", deadline, hasDL, called)
	}
}

// A shutdown signal cancels a running purge chunk; Run returns without further purge attempts.
func TestRelay_ShutdownDuringPurgeReturnsPromptly(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	var seen error
	src := &purgeFake{purge: func(ctx context.Context, _ int) (int64, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		seen = ctx.Err()
		return 0, ctx.Err()
	}}
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, m []outbox.Message) []error { return each(m, nil) })))
	<-entered
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if events, _ := src.snapshot(); countPurges(events) != 1 {
		t.Fatalf("purge calls = %d, want 1: %v", countPurges(events), events)
	}
	if !errors.Is(seen, context.Canceled) {
		t.Fatalf("purge context error = %v, want context.Canceled", seen)
	}
}

// deadlineSource records its own context deadline and the one its publish callback receives.
type deadlineSource struct {
	memSource
	mu                 sync.Mutex
	process, published time.Time
}

func (s *deadlineSource) Process(ctx context.Context, _ int, publish func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	dl, _ := ctx.Deadline()
	publish(ctx, messages(1))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.process.IsZero() {
		s.process = dl
	}
	return outbox.Result{}, nil
}

// Publishing gets at most 25s of the 30s batch, so marking and committing results keep at least 5s.
func TestRelay_PublishLeavesTimeToRecordResults(t *testing.T) {
	src := &deadlineSource{}
	stop := runRelay(newRelay(src, sinkFunc(func(ctx context.Context, m []outbox.Message) []error {
		dl, _ := ctx.Deadline()
		src.mu.Lock()
		if src.published.IsZero() {
			src.published = dl
		}
		src.mu.Unlock()
		return each(m, nil)
	})))
	waitFor(t, func() bool { src.mu.Lock(); defer src.mu.Unlock(); return !src.process.IsZero() })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if gap := src.process.Sub(src.published); gap < 5*time.Second {
		t.Fatalf("batch deadline - publish deadline = %v, want at least 5s", gap)
	}
}

// Shutdown during a purge must not start another batch, even while a backlog remains.
func TestRelay_ShutdownDuringPurgeStartsNoNewBatch(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	src := &purgeFake{full: true, purge: func(ctx context.Context, _ int) (int64, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	stop := runRelay(newRelay(src, sinkFunc(func(_ context.Context, m []outbox.Message) []error { return each(m, nil) })))
	<-entered
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	events, _ := src.snapshot()
	if events[len(events)-1] != "G" {
		t.Fatalf("events = %v, want no batch after the interrupted purge", events)
	}
}

// warnRecorder captures WARN messages.
type warnRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (w *warnRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (w *warnRecorder) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelWarn {
		w.mu.Lock()
		w.msgs = append(w.msgs, r.Message)
		w.mu.Unlock()
	}
	return nil
}
func (w *warnRecorder) WithAttrs([]slog.Attr) slog.Handler { return w }
func (w *warnRecorder) WithGroup(string) slog.Handler      { return w }

// Running out of the purge budget after deleting rows means rows remain, not a failure: the next
// turn runs on the next loop iteration, without a WARN or the 1-minute failure delay.
func TestRelay_PurgeBudgetExhaustedAfterProgressContinuesNextIteration(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	src := &purgeFake{purge: func(ctx context.Context, limit int) (int64, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch {
		case n <= 2:
			return int64(limit), nil
		case n == 3: // Slow chunk: still running when the 5s budget ends.
			<-ctx.Done()
			return 0, ctx.Err()
		default:
			return 0, nil
		}
	}}
	warns := &warnRecorder{}
	r := newRelay(src, sinkFunc(func(_ context.Context, m []outbox.Message) []error { return each(m, nil) }))
	r.Logger = slog.New(warns)
	stop := runRelay(r)
	deadline := time.Now().Add(8 * time.Second) // Covers the 5s budget; far below the 1-minute delay.
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("purge calls = %d, want a next turn soon after the budget ran out", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	warns.mu.Lock()
	defer warns.mu.Unlock()
	if len(warns.msgs) != 0 {
		t.Fatalf("warnings = %v, want none for an exhausted budget", warns.msgs)
	}
}
