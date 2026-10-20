package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/outbox"
)

// Loại sự kiện. Schema và cam kết tương thích: docs/events.md.
const (
	eventUserRegistered = "user.registered"
	eventUserUpdated    = "user.updated"
	eventUserDeleted    = "user.deleted"
)

// userEvent là sự kiện "mỏng": chỉ ID, version và thời điểm, không có PII (email, name).
// Consumer cần chi tiết thì gọi API. Xoá user vì vậy không đòi hỏi xoá gì trên broker.
type userEvent struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	EventType     string    `json:"event_type"`
	OccurredAt    time.Time `json:"occurred_at"`
	UserID        string    `json:"user_id"`
	Version       int64     `json:"version"`
}

// querier là phần chung của pool và transaction mà các lệnh ghi cần.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// write chạy fn trong một transaction khi outbox bật (thay đổi dữ liệu và sự kiện cùng commit
// hoặc cùng rollback), hoặc thẳng trên pool khi tắt.
func (r *UserRepository) write(ctx context.Context, fn func(q querier) error) error {
	if !r.outbox {
		return fn(r.pool)
	}
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error { return fn(tx) })
}

// appendEvent ghi sự kiện vào outbox bằng q (transaction của lần ghi). Trace context của request
// được lưu vào headers, để trace đi tiếp qua broker tới consumer.
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
	headers := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, headers)
	_, err = q.Exec(ctx,
		`INSERT INTO outbox (id, aggregate_id, event_type, payload, headers) VALUES ($1, $2, $3, $4, $5)`,
		eventID.String(), id.String(), eventType, payload, map[string]string(headers))
	return err
}

// OutboxSource hiện thực outbox.Source trên bảng outbox.
type OutboxSource struct {
	pool *pgxpool.Pool
}

var _ outbox.Source = (*OutboxSource)(nil)

func NewOutboxSource(pool *pgxpool.Pool) *OutboxSource { return &OutboxSource{pool: pool} }

// Process khoá một lô bằng FOR UPDATE SKIP LOCKED: nhiều relay chạy song song không lấy trùng
// message, và không chờ nhau. Khoá được giữ tới khi publish xong và lô được đánh dấu.
func (s *OutboxSource) Process(ctx context.Context, limit int, publish func(context.Context, []outbox.Message) error) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, aggregate_id::text, event_type, payload, headers FROM outbox
			WHERE published_at IS NULL ORDER BY created_at, id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
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
		if err := publish(ctx, msgs); err != nil {
			return err
		}
		ids := make([]string, len(msgs))
		for i, m := range msgs {
			ids[i] = m.ID
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET published_at = now() WHERE id = ANY($1::uuid[])`, ids); err != nil {
			return err
		}
		n = len(msgs)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("outbox: %w", err)
	}
	return n, nil
}

func (s *OutboxSource) Purge(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM outbox WHERE published_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("outbox purge: %w", err)
	}
	return tag.RowsAffected(), nil
}
