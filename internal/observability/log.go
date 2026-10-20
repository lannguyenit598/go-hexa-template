// Package observability provides shared infrastructure for request-context logging and
// OpenTelemetry (traces, metrics). It depends only on the standard library and OpenTelemetry,
// not adapters, so all layers can log through slog.*Context(ctx, ...).
package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type logAttrsKey struct{}

// WithLogAttrs attaches attributes to ctx; every log using this context through ContextHandler
// automatically includes them, for example request_id.
func WithLogAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	existing, _ := ctx.Value(logAttrsKey{}).([]slog.Attr)
	merged := make([]slog.Attr, 0, len(existing)+len(attrs))
	merged = append(merged, existing...)
	merged = append(merged, attrs...)
	return context.WithValue(ctx, logAttrsKey{}, merged)
}

// ContextHandler wraps a slog.Handler, adding to each record:
//   - attributes attached with WithLogAttrs (request_id...);
//   - the current span's trace_id and span_id for navigation between logs and traces.
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
