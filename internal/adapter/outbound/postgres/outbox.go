package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/propagation"

	"github.com/example/userapi/internal/adapter/outbound/postgres/sqlcgen"
	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/outbox"
)

// Event types. Schema and compatibility guarantees: docs/events.md.
const (
	eventUserRegistered = "user.registered"
	eventUserUpdated    = "user.updated"
	eventUserDeleted    = "user.deleted"
)

// userEvent is a thin event: IDs, version, and time, without email or name.
// Consumers fetch details from the API. Deleting a user does not require deleting their name/email from the broker.
type userEvent struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	OccurredAt    time.Time `json:"occurred_at"`
	UserID        string    `json:"user_id"`
	Version       int64     `json:"version"`
}

// write executes fn in a transaction when outbox is enabled (data and event commit or
// roll back together), or directly on the pool when disabled. fn must run every query through q,
// which is bound to that transaction: r.queries would run outside it.
func (r *UserRepository) write(ctx context.Context, fn func(q *sqlcgen.Queries) error) error {
	if !r.outbox {
		return fn(r.queries)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return fn(r.queries.WithTx(tx)) })
}

// appendEvent writes through q, the mutation's transaction. Request trace context is saved
// in headers so tracing continues through the broker to consumers.
func (r *UserRepository) appendEvent(ctx context.Context, q *sqlcgen.Queries, eventType string, id domain.UserID, version int64, at time.Time) error {
	if !r.outbox {
		return nil
	}
	eventID, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("outbox event id: %w", err)
	}
	payload, err := json.Marshal(userEvent{
		SchemaVersion: 1,
		EventID:       eventID.String(),
		EventType:     eventType,
		OccurredAt:    at.UTC(),
		UserID:        id.String(),
		Version:       version,
	})
	if err != nil {
		return fmt.Errorf("outbox payload: %w", err)
	}
	// Use only trace context (traceparent/tracestate), not the global propagator that includes Baggage:
	// client-supplied baggage may contain personal data or arbitrary values.
	headers := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, headers)
	encodedHeaders, err := json.Marshal(headers) // Never nil: no trace context is stored as {}.
	if err != nil {
		return fmt.Errorf("outbox headers: %w", err)
	}
	// created_at uses clock_timestamp() (INSERT time), not now() (transaction start time).
	// The INSERT runs after the mutation has locked the user row, so that user's created_at order
	// matches commit order even when transactions started in a different order.
	return q.InsertOutboxEvent(ctx, sqlcgen.InsertOutboxEventParams{
		ID:          eventID.String(),
		AggregateID: id.String(),
		EventType:   eventType,
		Payload:     payload,
		Headers:     encodedHeaders,
	})
}

// OutboxSource implements outbox.Source using the outbox table.
type OutboxSource struct {
	pool    *pgxpool.Pool
	queries *sqlcgen.Queries
}

var _ outbox.Source = (*OutboxSource)(nil)

func NewOutboxSource(pool *pgxpool.Pool) *OutboxSource {
	return &OutboxSource{pool: pool, queries: sqlcgen.New(pool)}
}

// Process locks a batch with FOR UPDATE SKIP LOCKED: concurrent relays do not select the same
// message or wait for each other. Locks remain until publishing and per-message result updates finish.
func (s *OutboxSource) Process(ctx context.Context, limit int, publish func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	var res outbox.Result
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = outbox.Result{}
		q := s.queries.WithTx(tx)
		rows, err := q.LockPendingOutboxEvents(ctx, int64(limit))
		if err != nil || len(rows) == 0 {
			return err
		}
		msgs := make([]outbox.Message, len(rows))
		for i, row := range rows {
			msgs[i] = outbox.Message{ID: row.ID, Key: row.AggregateID, Type: row.EventType, Payload: row.Payload}
			if err := json.Unmarshal(row.Headers, &msgs[i].Headers); err != nil {
				return fmt.Errorf("decode headers of %s: %w", row.ID, err)
			}
		}
		errs := publish(ctx, msgs)
		if len(errs) != len(msgs) {
			return fmt.Errorf("sink returned %d results for %d messages", len(errs), len(msgs))
		}
		var published []string
		for i, m := range msgs {
			switch err := errs[i]; {
			case err == nil:
				published = append(published, m.ID)
			case errors.Is(err, outbox.ErrPermanent):
				if err := q.ParkOutboxEvent(ctx, sqlcgen.ParkOutboxEventParams{ID: m.ID, LastError: err.Error()}); err != nil {
					return err
				}
				res.Parked = append(res.Parked, m.ID)
			default:
				res.Retry++
			}
			if errs[i] != nil && res.Err == nil {
				res.Err = errs[i]
			}
		}
		if len(published) > 0 {
			if err := q.MarkOutboxEventsPublished(ctx, published); err != nil {
				return err
			}
		}
		res.Published = len(published)
		return nil
	})
	if err != nil {
		return outbox.Result{}, fmt.Errorf("outbox: %w", err)
	}
	return res, nil
}

func (s *OutboxSource) Purge(ctx context.Context, before time.Time, limit int) (int64, error) {
	n, err := s.queries.PurgePublishedOutboxEvents(ctx, sqlcgen.PurgePublishedOutboxEventsParams{Before: before, MaxRows: int64(limit)})
	if err != nil {
		return 0, fmt.Errorf("outbox purge: %w", err)
	}
	return n, nil
}
