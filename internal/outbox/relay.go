// Package outbox forwards events from an outbox table, written atomically with data,
// to a message broker. It knows neither PostgreSQL nor Kafka; it defines
// Source and Sink for adapters to implement. Delivery is at-least-once: consumers must
// deduplicate by event_id or version (see docs/events.md).
package outbox

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"
)

// Message is an outbox event ready for delivery to the broker.
type Message struct {
	ID      string // event_id (UUIDv7)
	Key     string // Aggregate ID: broker partition key; see docs/events.md for ordering limits.
	Type    string // For example, "user.updated".
	Payload []byte // JSON; schema in docs/events.md
	Headers map[string]string
}

// ErrPermanent marks errors that retries cannot fix, such as broker rejection of
// oversized or invalid content. Sink wraps it with %w; Source parks the message
// instead of retrying forever and blocking later messages.
var ErrPermanent = errors.New("outbox: message rejected permanently")

// Result contains the outcome of processing one batch.
type Result struct {
	Published int      // Accepted by the broker and marked published.
	Parked    []string // IDs permanently rejected and parked.
	Retry     int      // Temporary failures: keep and retry later.
	Err       error    // First publish error in the batch, if any, for logging.
}

// Source stores outbox messages.
type Source interface {
	// Process selects up to limit unpublished messages in write order, calls publish, and handles
	// each result in one transaction: mark successes as published, park errors wrapping
	// ErrPermanent, and leave other failures for retry. The returned error describes a failure
	// of batch processing itself; per-message publish failures are reported in Result.
	Process(ctx context.Context, limit int, publish func(context.Context, []Message) []error) (Result, error)
	// Purge deletes at most limit messages published before before and returns how many it
	// deleted. Parked messages are retained.
	Purge(ctx context.Context, before time.Time, limit int) (int64, error)
}

// Sink sends messages to the broker. Publish returns one error per message in input order:
// errs[i] == nil means the broker accepted msgs[i].
type Sink interface {
	Publish(ctx context.Context, msgs []Message) []error
}

const (
	// batchTimeout bounds a batch, publishing plus recording its results and committing; shutdown
	// signals do not cancel an active batch (see Run). publishTimeout bounds publishing alone,
	// leaving the rest for recording results, so a slow broker cannot expire the commit.
	batchTimeout   = 30 * time.Second
	publishTimeout = 25 * time.Second
	purgeEvery     = time.Hour
	// A purge turn deletes up to maxPurgeChunks chunks of purgeChunkSize rows within purgeBudget,
	// so cleanup never takes over the loop; a larger backlog continues in the next iteration.
	purgeChunkSize  = 1000
	maxPurgeChunks  = 10
	purgeBudget     = 5 * time.Second
	purgeRetryDelay = time.Minute
	// healthSlack allows time for purge (purgeBudget must fit inside it) and scheduling delays
	// in HealthThreshold.
	healthSlack = 30 * time.Second
)

// Relay reads outbox batches and forwards them to Sink until ctx is canceled.
type Relay struct {
	Source       Source
	Sink         Sink
	Logger       *slog.Logger
	BatchSize    int
	PollInterval time.Duration
	Retention    time.Duration

	lastActivity atomic.Int64 // UnixNano of the last loop progress; 0 = not started.
}

// LastActivity is the last loop progress (batch start or completion); zero before startup.
func (r *Relay) LastActivity() time.Time {
	if n := r.lastActivity.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

// HealthThreshold is the longest healthy silence: the maximum batch duration, a poll interval,
// and slack. A longer silence indicates a stalled loop.
func (r *Relay) HealthThreshold() time.Duration {
	return batchTimeout + r.PollInterval + healthSlack
}

// Healthy reports whether the relay has started and progressed within HealthThreshold.
func (r *Relay) Healthy(now time.Time) bool {
	last := r.LastActivity()
	return !last.IsZero() && now.Sub(last) <= r.HealthThreshold()
}

func (r *Relay) beat() { r.lastActivity.Store(time.Now().UnixNano()) }

// Run loops until ctx is canceled, returning nil. Publish errors are logged and retried
// after polling, so temporary broker outages do not stop Run.
//
// An in-flight batch finishes during shutdown (up to batchTimeout): canceling midway can
// leave delivered messages unmarked, causing redelivery on the next run.
func (r *Relay) Run(ctx context.Context) error {
	var nextDue time.Time // Zero: purge after the first batch.
	for {
		r.beat()
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		res, err := r.Source.Process(batchCtx, r.BatchSize, r.publish)
		cancel()
		r.beat()
		r.log(ctx, res, err)
		if ctx.Err() != nil {
			return nil
		}
		// Before the backlog check: a backlog that never drains must not postpone purging forever.
		if !time.Now().Before(nextDue) {
			nextDue = r.purge(ctx)
			if ctx.Err() != nil { // Shutdown during the purge: start no new batch.
				return nil
			}
		}
		if err == nil && res.Retry == 0 && res.Published+len(res.Parked) == r.BatchSize {
			continue // Backlog remains: read the next batch immediately.
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.PollInterval):
		}
	}
}

// publish bounds Sink.Publish by publishTimeout, ending at least batchTimeout-publishTimeout before
// the batch deadline so recording results always keeps that reserve.
func (r *Relay) publish(ctx context.Context, msgs []Message) []error {
	deadline := time.Now().Add(publishTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Add(publishTimeout-batchTimeout).Before(deadline) {
		deadline = dl.Add(publishTimeout - batchTimeout)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return r.Sink.Publish(ctx, msgs)
}

// purge runs one bounded turn and returns when the next turn is due.
func (r *Relay) purge(ctx context.Context) time.Time {
	budgetCtx, cancel := context.WithTimeout(ctx, purgeBudget)
	defer cancel()
	before := time.Now().Add(-r.Retention)
	var total int64
	var err error
	for range maxPurgeChunks {
		var n int64
		n, err = r.Source.Purge(budgetCtx, before, purgeChunkSize)
		r.beat()
		total += n
		if err != nil || n < purgeChunkSize {
			break
		}
	}
	more := total == purgeChunkSize*maxPurgeChunks
	if err != nil && total > 0 && budgetCtx.Err() != nil && ctx.Err() == nil {
		err, more = nil, true // The budget ran out after progress: rows remain, nothing failed.
	}
	if err != nil && ctx.Err() == nil {
		r.Logger.WarnContext(ctx, "outbox relay: purge failed", "error", err)
	}
	if total > 0 {
		r.Logger.InfoContext(ctx, "outbox relay: purged published messages", "count", total)
	}
	return nextPurge(time.Now(), more, err)
}

// nextPurge schedules the next purge turn: soon after a failure, at once while rows remain
// (the turn ended on its chunk or time limit), otherwise after purgeEvery.
func nextPurge(now time.Time, more bool, err error) time.Time {
	switch {
	case err != nil:
		return now.Add(purgeRetryDelay)
	case more:
		return now
	default:
		return now.Add(purgeEvery)
	}
}

func (r *Relay) log(ctx context.Context, res Result, err error) {
	if err != nil {
		r.Logger.WarnContext(ctx, "outbox relay: batch failed, will retry", "error", err)
		return
	}
	if len(res.Parked) > 0 {
		// Operator action required: consumers will not receive these events (see docs/operations.md).
		r.Logger.ErrorContext(ctx, "outbox relay: parked messages the broker rejected permanently",
			"count", len(res.Parked), "ids", res.Parked, "error", res.Err)
	}
	if res.Retry > 0 {
		r.Logger.WarnContext(ctx, "outbox relay: publish failed, will retry", "count", res.Retry, "error", res.Err)
	}
	if res.Published > 0 {
		r.Logger.DebugContext(ctx, "outbox relay: published", "count", res.Published)
	}
}
