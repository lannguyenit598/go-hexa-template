package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/propagation"

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

// querier is the pool/transaction interface needed by write operations.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// write executes fn in a transaction when outbox is enabled (data and event commit or
// roll back together), or directly on the pool when disabled.
func (r *UserRepository) write(ctx context.Context, fn func(q querier) error) error {
	if !r.outbox {
		return fn(r.pool)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return fn(tx) })
}

// appendEvent writes through q, the mutation's transaction. Request trace context is saved
// in headers so tracing continues through the broker to consumers.
func (r *UserRepository) appendEvent(ctx context.Context, q querier, eventType string, id domain.UserID, version int64, at time.Time) error {
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
	// created_at uses clock_timestamp() (INSERT time), not now() (transaction start time).
	// The INSERT runs after the mutation has locked the user row, so that user's created_at order
	// matches commit order even when transactions started in a different order.
	_, err = q.Exec(ctx,
		`INSERT INTO outbox (id, aggregate_id, event_type, payload, headers, created_at)
		VALUES ($1, $2, $3, $4, $5, clock_timestamp())`,
		eventID.String(), id.String(), eventType, payload, map[string]string(headers))
	return err
}

// OutboxSource implements outbox.Source using the outbox table.
type OutboxSource struct {
	pool *pgxpool.Pool
}

var _ outbox.Source = (*OutboxSource)(nil)

func NewOutboxSource(pool *pgxpool.Pool) *OutboxSource { return &OutboxSource{pool: pool} }

// Process locks a batch with FOR UPDATE SKIP LOCKED: concurrent relays do not select the same
// message or wait for each other. Locks remain until publishing and per-message result updates finish.
func (s *OutboxSource) Process(ctx context.Context, limit int, publish func(context.Context, []outbox.Message) []error) (outbox.Result, error) {
	var res outbox.Result
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res = outbox.Result{}
		rows, err := tx.Query(ctx, `SELECT id::text, aggregate_id::text, event_type, payload, headers FROM outbox
			WHERE published_at IS NULL AND parked_at IS NULL ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		msgs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (outbox.Message, error) {
			var m outbox.Message
			err := row.Scan(&m.ID, &m.Key, &m.Type, &m.Payload, &m.Headers)
			return m, err
		})
		if err != nil || len(msgs) == 0 {
			return err
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
				if _, err := tx.Exec(ctx, `UPDATE outbox SET parked_at = now(), last_error = $2 WHERE id = $1`, m.ID, err.Error()); err != nil {
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
			if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1::uuid[])`, published); err != nil {
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

func (s *OutboxSource) Purge(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM outbox WHERE published_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("outbox purge: %w", err)
	}
	return tag.RowsAffected(), nil
}
