package httpapi_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/observability"
)

func TestTracingAndRequestScopedLogs(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	var logs bytes.Buffer
	logger := slog.New(observability.NewContextHandler(slog.NewJSONHandler(&logs, nil)))
	api := newAPIWith(t, httpapi.Options{Logger: logger})
	h := httpapi.Instrument(api.h)

	serve := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	serve(http.MethodGet, "/healthz")
	w := serve(http.MethodGet, "/api/v1/users/0192f3a4-5b6c-7d8e-9f01-23456789abcd")
	expectStatus(t, w, http.StatusNotFound)

	spans := rec.Ended()
	if len(spans) != 1 {
		names := make([]string, len(spans))
		for i, s := range spans {
			names[i] = s.Name()
		}
		t.Fatalf("want exactly 1 span (probes are not traced), got %v", names)
	}
	span := spans[0]
	if span.Name() != "GET /api/v1/users/:id" {
		t.Fatalf("span name = %q, want route-based name", span.Name())
	}

	// Dòng access log mang request_id (cũng có trong header) và trace_id của span.
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatalf("log: %v (%s)", err, logs.String())
	}
	if line["request_id"] != w.Header().Get("X-Request-Id") || line["request_id"] == "" {
		t.Errorf("request_id = %v, header = %q", line["request_id"], w.Header().Get("X-Request-Id"))
	}
	if line["trace_id"] != span.SpanContext().TraceID().String() {
		t.Errorf("trace_id = %v, want %s", line["trace_id"], span.SpanContext().TraceID())
	}
	if strings.Count(logs.String(), "request_id") != 1 {
		t.Errorf("request_id must appear exactly once: %s", logs.String())
	}
}
