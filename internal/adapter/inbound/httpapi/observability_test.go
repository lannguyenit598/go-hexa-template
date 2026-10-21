package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
		h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), method, path, nil))
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

	// The access log includes request_id (also in the header) and the span trace_id.
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

func TestAccessLogOmitsQueryString(t *testing.T) {
	var logs bytes.Buffer
	api := newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})

	w := httptest.NewRecorder()
	api.h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/api/v1/users?limit=1&email=alice@example.com&token=private-token", nil))
	expectStatus(t, w, http.StatusOK)

	for _, secret := range []string{"private-token", "alice@example.com"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("access log leaks query value %q: %s", secret, logs.String())
		}
	}
	var line map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &line); err != nil {
		t.Fatalf("log: %v (%s)", err, logs.String())
	}
	if line["path"] != "/api/v1/users" || line["route"] != "/api/v1/users" {
		t.Errorf("path = %v, route = %v; want /api/v1/users for both", line["path"], line["route"])
	}
}

func TestTracing_EveryOperationIsNamedByRoute(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	h := httpapi.Instrument(newAPI(t, nil).h)
	id := "/api/v1/users/0192f3a4-5b6c-7d8e-9f01-23456789abcd"
	for _, tc := range []struct{ method, path, body, span string }{
		{http.MethodPost, "/api/v1/users", `{"email":"x"}`, "POST /api/v1/users"},
		{http.MethodGet, "/api/v1/users?limit=x", "", "GET /api/v1/users"},
		{http.MethodGet, id, "", "GET /api/v1/users/:id"},
		{http.MethodPatch, id, `{}`, "PATCH /api/v1/users/:id"},
		{http.MethodDelete, id, "", "DELETE /api/v1/users/:id"},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)

		spans := rec.Ended()
		span := spans[len(spans)-1]
		attrs := map[string]any{}
		for _, kv := range span.Attributes() {
			attrs[string(kv.Key)] = kv.Value.AsInterface()
		}
		if span.Name() != tc.span || attrs["http.route"] != tc.span[len(tc.method)+1:] {
			t.Errorf("%s %s: span %q, http.route %v; want %q", tc.method, tc.path, span.Name(), attrs["http.route"], tc.span)
		}
		if attrs["http.response.status_code"] != int64(w.Code) {
			t.Errorf("%s %s: span status %v, response %d", tc.method, tc.path, attrs["http.response.status_code"], w.Code)
		}
	}
	if n := len(rec.Ended()); n != 5 {
		t.Errorf("want one span per request, got %d", n)
	}
}

func TestAccessLog_ServerErrorIsLoggedOnceWithoutLeaking(t *testing.T) {
	var logs bytes.Buffer
	api := &apiClient{t: t, h: httpapi.NewServer(failingUsers{errors.New("dial tcp: password=secret")},
		httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})}

	w := api.do(http.MethodGet, "/api/v1/users", "")
	expectStatus(t, w, http.StatusInternalServerError)
	if strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("response leaks the internal error: %s", w.Body.String())
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want exactly one log entry, got %d: %s", len(lines), logs.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatal(err)
	}
	if line["msg"] != "http_request" || line["level"] != "ERROR" || line["status"] != float64(500) ||
		line["error"] != "dial tcp: password=secret" {
		t.Fatalf("access log entry = %v", line)
	}
}
