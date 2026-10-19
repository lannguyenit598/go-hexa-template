// Package observability gom phần hạ tầng quan sát dùng chung: log theo ngữ cảnh request và
// OpenTelemetry (trace, metric). Chỉ phụ thuộc thư viện chuẩn và OpenTelemetry, không phụ
// thuộc adapter nào, nên mọi tầng đều có thể log bằng slog.*Context(ctx, ...).
package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type logAttrsKey struct{}

// WithLogAttrs gắn thêm attribute vào ctx; mọi dòng log ghi bằng ctx này (qua ContextHandler)
// sẽ tự mang các attribute đó, ví dụ request_id.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// ContextHandler bọc một slog.Handler, thêm vào mỗi bản ghi:
//   - attribute đã gắn bằng WithLogAttrs (request_id...);
//   - trace_id và span_id của span hiện tại, để nhảy từ log sang trace và ngược lại.
type ContextHandler struct {
	slog.Handler
}

func NewContextHandler(h slog.Handler) *ContextHandler { return &ContextHandler{Handler: h} }

func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if attrs, ok := ctx.Value(logAttrsKey{}).([]slog.Attr); ok {
		r.AddAttrs(attrs...)
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{Handler: h.Handler.WithGroup(name)}
}
