package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v5"
)

type ctxKey struct{}

// TestHumaAdapter_Contract exercises every huma.Context method the adapter implements, through
// a real Echo router, group middleware, and request.
func TestHumaAdapter_Contract(t *testing.T) {
	e := echo.New()
	var groupCalls int
	var sawValue any
	group := e.Group("/api/v1", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			groupCalls++
			err := next(c)
			sawValue = c.Request().Context().Value(ctxKey{}) // Set by the handler through WithContext.
			return err
		}
	})
	adapter := &humaAdapter{Echo: e, group: group, prefix: "/api/v1"}
	handlerErr := errors.New("handler failed")
	var returned error
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { returned = next(c); return nil }
	})

	op := &huma.Operation{Method: http.MethodPost, Path: "/api/v1/things/{id}"}
	adapter.Handle(op, func(ctx huma.Context) {
		u := ctx.URL()
		checks := map[string][2]any{
			"Operation":  {ctx.Operation(), op},
			"Method":     {ctx.Method(), http.MethodPost},
			"Host":       {ctx.Host(), "example.com"},
			"RemoteAddr": {ctx.RemoteAddr(), "192.0.2.1:1234"},
			"URL":        {u.EscapedPath() + "?" + u.RawQuery, "/api/v1/things/a%2Fb?q=1&q=2"},
			"Param":      {ctx.Param("id"), "a%2Fb"},
			"Query":      {ctx.Query("q"), "1"},
			"Header":     {ctx.Header("X-Multi"), "one"},
			"Accept":     {ctx.Header("accept"), ""}, // Hidden from content negotiation.
			"TLS":        {ctx.TLS() == nil, true},
			"Version":    {ctx.Version().Proto, "HTTP/1.1"},
			"Context":    {ctx.Context().Value(ctxKey{}), nil},
		}
		for name, c := range checks {
			if c[0] != c[1] {
				t.Errorf("%s = %v, want %v", name, c[0], c[1])
			}
		}
		var multi []string
		ctx.EachHeader(func(name, value string) {
			if name == "X-Multi" {
				multi = append(multi, value)
			}
		})
		if strings.Join(multi, ",") != "one,two" {
			t.Errorf("EachHeader X-Multi = %v", multi)
		}
		if body, err := io.ReadAll(ctx.BodyReader()); err != nil || string(body) != "payload" {
			t.Errorf("BodyReader = %q, %v", body, err)
		}

		derived := huma.WithValue(ctx, ctxKey{}, "v")
		if derived.Context().Value(ctxKey{}) != "v" {
			t.Error("WithContext must replace the request context")
		}
		derived.SetHeader("X-Set", "a")
		derived.AppendHeader("X-Append", "b")
		derived.AppendHeader("X-Append", "c")
		derived.SetStatus(http.StatusAccepted)
		if ctx.Status() != http.StatusAccepted {
			t.Errorf("Status seen by the original context = %d", ctx.Status())
		}
		_, _ = io.WriteString(derived.BodyWriter(), "done") // skip: the recorder does not fail.
		reportError(derived, handlerErr)
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.com/api/v1/things/a%2Fb?q=1&q=2", strings.NewReader("payload"))
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")
	req.Header.Set("Accept", "json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted || rec.Body.String() != "done" || rec.Header().Get("X-Set") != "a" ||
		strings.Join(rec.Header().Values("X-Append"), ",") != "b,c" {
		t.Errorf("response %d %q, headers %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if groupCalls != 1 || sawValue != "v" {
		t.Errorf("group middleware ran %d times and saw %v; want once with the derived context value", groupCalls, sawValue)
	}
	if !errors.Is(returned, handlerErr) {
		t.Errorf("Echo received %v, want the reported handler error", returned)
	}

	// Operations outside the prefix would skip APIMiddleware: registering one fails.
	for _, path := range []string{"/docs", "/api/v1", "/api/v1users"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Handle(%q) must panic", path)
				}
			}()
			adapter.Handle(&huma.Operation{Method: http.MethodGet, Path: path}, func(huma.Context) {})
		}()
	}
}

// When a client disconnects mid-body, Huma sets status 499 after the status line was sent. That
// must not reach Echo, which logs a second WriteHeader at ERROR (a false alert, like 5xx for 499).
func TestHumaAdapter_StatusAfterCommitIsNotWritten(t *testing.T) {
	var logs bytes.Buffer
	e := echo.New()
	e.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	adapter := &humaAdapter{Echo: e, group: e.Group("/api/v1"), prefix: "/api/v1"}
	adapter.Handle(&huma.Operation{Method: http.MethodGet, Path: "/api/v1/things"}, func(ctx huma.Context) {
		ctx.SetStatus(http.StatusOK)
		_, _ = io.WriteString(ctx.BodyWriter(), "partial") // skip: the recorder does not fail.
		ctx.SetStatus(StatusClientClosedRequest)
		if ctx.Status() != StatusClientClosedRequest {
			t.Errorf("Status = %d, want the last status set", ctx.Status())
		}
	})
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/things", nil))
	if rec.Code != http.StatusOK || logs.Len() != 0 {
		t.Errorf("status %d, logs %q; want 200 and no log", rec.Code, logs.String())
	}
}
