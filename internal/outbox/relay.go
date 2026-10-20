// Package outbox chuyển sự kiện từ bảng outbox (ghi cùng transaction với dữ liệu) lên message
// broker: transactional outbox. Package này không biết PostgreSQL hay Kafka; nó chỉ định nghĩa
// Source và Sink, và adapter hiện thực chúng. Giao ít nhất một lần: consumer phải bỏ qua bản
// trùng theo event_id hoặc version (xem docs/events.md).
package outbox

import (
	"context"
	"errors"
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

// ErrPermanent đánh dấu lỗi mà gửi lại cũng không thành công, ví dụ broker từ chối message vì
// nội dung (quá lớn, không hợp lệ). Sink bọc lỗi đó bằng %w; Source tách message ra khỏi hàng đợi
// (parked) thay vì thử lại mãi và chặn mọi message phía sau.
var ErrPermanent = errors.New("outbox: message rejected permanently")

// Result là kết quả xử lý một lô.
type Result struct {
	Published int      // broker đã nhận, đã đánh dấu
	Parked    []string // ID message bị từ chối vĩnh viễn, đã tách khỏi hàng đợi
	Retry     int      // lỗi tạm thời: giữ nguyên, thử lại ở lần sau
	Err       error    // lỗi publish đầu tiên trong lô (nếu có), để log
}

// Source là nơi lưu outbox.
type Source interface {
	// Process giữ tối đa limit message chưa publish (theo thứ tự ghi), gọi publish, rồi xử lý kết
	// quả của TỪNG message trong cùng transaction: thành công thì đánh dấu đã publish, lỗi bọc
	// ErrPermanent thì tách riêng, lỗi khác thì để nguyên cho lần sau. Lỗi trả về chỉ là lỗi của
	// chính Source (database).
	Process(ctx context.Context, limit int, publish func(context.Context, []Message) []error) (Result, error)
	// Purge xoá message đã publish trước thời điểm before. Message đã tách riêng không bị xoá.
	Purge(ctx context.Context, before time.Time) (int64, error)
}

// Sink gửi message lên broker. Publish trả lỗi theo từng message, cùng thứ tự với msgs:
// errs[i] == nil nghĩa là broker đã nhận msgs[i].
type Sink interface {
	Publish(ctx context.Context, msgs []Message) []error
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
		res, err := r.Source.Process(batchCtx, r.BatchSize, r.Sink.Publish)
		cancel()
		r.log(ctx, res, err)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && res.Retry == 0 && res.Published+len(res.Parked) == r.BatchSize {
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

func (r *Relay) log(ctx context.Context, res Result, err error) {
	if err != nil {
		r.Logger.WarnContext(ctx, "outbox relay: batch failed, will retry", "error", err)
		return
	}
	if len(res.Parked) > 0 {
		// Cần người xử lý: consumer sẽ không nhận các sự kiện này (xem docs/operations.md).
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
