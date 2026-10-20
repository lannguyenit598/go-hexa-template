// Package outbox chuyển sự kiện từ bảng outbox (ghi cùng transaction với dữ liệu) lên message
// broker: transactional outbox. Package này không biết PostgreSQL hay Kafka; nó chỉ định nghĩa
// Source và Sink, và adapter hiện thực chúng. Giao ít nhất một lần: consumer phải bỏ qua bản
// trùng theo event_id hoặc version (xem docs/events.md).
package outbox

import (
	"context"
	"log/slog"
	"time"
)

// Message là một sự kiện đã nằm trong outbox, sẵn sàng gửi lên broker.
type Message struct {
	ID      string // event_id (UUIDv7)
	Key     string // ID của aggregate: broker giữ thứ tự theo key
	Type    string // ví dụ "user.updated"
	Payload []byte // JSON, schema trong docs/events.md
	Headers map[string]string
}

// Source là nơi lưu outbox.
type Source interface {
	// Process giữ tối đa limit message chưa publish (theo thứ tự ghi), gọi publish, và chỉ đánh
	// dấu đã publish khi publish thành công, tất cả trong một transaction: publish lỗi thì
	// message còn nguyên cho lần sau. Trả số message đã publish.
	Process(ctx context.Context, limit int, publish func(context.Context, []Message) error) (int, error)
	// Purge xoá message đã publish trước thời điểm before.
	Purge(ctx context.Context, before time.Time) (int64, error)
}

// Sink gửi message lên broker. Publish chỉ trả nil khi broker đã nhận mọi message.
type Sink interface {
	Publish(ctx context.Context, msgs []Message) error
}

const (
	// batchTimeout giới hạn một lô; lô đang chạy không bị huỷ theo tín hiệu tắt (xem Run).
	batchTimeout = 30 * time.Second
	purgeEvery   = time.Hour
)

// Relay đọc outbox theo lô và đẩy lên Sink cho tới khi ctx bị huỷ.
type Relay struct {
	Source       Source
	Sink         Sink
	Logger       *slog.Logger
	BatchSize    int
	PollInterval time.Duration
	Retention    time.Duration
}

// Run chạy vòng lặp relay; trả nil khi ctx bị huỷ. Lỗi publish được log và thử lại ở lần poll
// sau, nên Run không dừng vì broker tạm thời không tới được.
//
// Lô đang publish khi có tín hiệu tắt vẫn được chạy hết (tối đa batchTimeout): huỷ giữa chừng
// sẽ khiến message đã tới broker không được đánh dấu, và bị gửi lại ở lần chạy sau.
func (r *Relay) Run(ctx context.Context) error {
	var lastPurge time.Time
	for {
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		n, err := r.Source.Process(batchCtx, r.BatchSize, r.Sink.Publish)
		cancel()
		switch {
		case err != nil:
			r.Logger.WarnContext(ctx, "outbox relay: publish failed, will retry", "error", err)
		case n > 0:
			r.Logger.DebugContext(ctx, "outbox relay: published", "count", n)
		}
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && n == r.BatchSize {
			continue // còn tồn đọng: đọc lô tiếp ngay
		}
		if time.Since(lastPurge) >= purgeEvery {
			lastPurge = time.Now()
			if deleted, err := r.Source.Purge(ctx, lastPurge.Add(-r.Retention)); err != nil {
				r.Logger.WarnContext(ctx, "outbox relay: purge failed", "error", err)
			} else if deleted > 0 {
				r.Logger.InfoContext(ctx, "outbox relay: purged published messages", "count", deleted)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.PollInterval):
		}
	}
}
