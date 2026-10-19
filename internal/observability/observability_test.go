package observability_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/example/userapi/internal/observability"
)

func TestContextHandler_AddsContextAttrsAndTraceIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(observability.NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("svc", "x")

	ctx := observability.WithLogAttrs(context.Background(), slog.String("request_id", "r-1"))
	ctx = observability.WithLogAttrs(ctx, slog.String("user_id", "u-1"))
	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{1, 2, 3},
		SpanID:     trace.SpanID{4, 5, 6},
		TraceFlags: trace.FlagsSampled,
	})
	ctx = trace.ContextWithSpanContext(ctx, sc)

	logger.InfoContext(ctx, "hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"svc":        "x",
		"request_id": "r-1",
		"user_id":    "u-1",
		"trace_id":   sc.TraceID().String(),
		"span_id":    sc.SpanID().String(),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (log: %s)", k, got[k], v, buf.String())
		}
	}
}

func TestContextHandler_NoContextNoExtraAttrs(t *testing.T) {
	var buf bytes.Buffer
	slog.New(observability.NewContextHandler(slog.NewJSONHandler(&buf, nil))).Info("plain")
	if bytes.Contains(buf.Bytes(), []byte("trace_id")) || bytes.Contains(buf.Bytes(), []byte("request_id")) {
		t.Fatalf("unexpected attrs: %s", buf.String())
	}
}

func TestEnabled(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_SDK_DISABLED": "TRUE"}, false},
	}
	for _, c := range cases {
		if got := observability.Enabled(env(c.env)); got != c.want {
			t.Errorf("Enabled(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}
