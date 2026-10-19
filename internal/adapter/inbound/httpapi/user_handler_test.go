package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/adapter/outbound/system"
	"github.com/example/userapi/internal/core/service"
)

type apiClient struct {
	t *testing.T
	h http.Handler
}

func newAPI(t *testing.T, ready func(context.Context) error) *apiClient {
	t.Helper()
	return newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Ready: ready})
}

func newAPIWith(t *testing.T, opts httpapi.Options) *apiClient {
	t.Helper()
	svc := service.NewUserService(memory.NewUserRepository(), system.NewUUIDv7(), system.Clock{})
	return &apiClient{t: t, h: httpapi.NewServer(svc, opts)}
}

func (a *apiClient) do(method, path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

type user struct {
	ID, Email, Name string
}

type problem struct {
	Status int    `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Errors []struct {
		Field string `json:"field"`
	} `json:"errors"`
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, want, rec.Body.String())
	}
}

func TestUserCRUDFlow(t *testing.T) {
	api := newAPI(t, nil)

	// Create
	rec := api.do(http.MethodPost, "/api/v1/users", `{"email":"Lan@Example.com","name":"Lan"}`)
	expectStatus(t, rec, http.StatusCreated)
	created := decode[user](t, rec)
	if created.Email != "lan@example.com" || created.ID == "" {
		t.Fatalf("unexpected %+v", created)
	}
	if loc := rec.Header().Get("Location"); loc != "/api/v1/users/"+created.ID {
		t.Fatalf("Location = %q", loc)
	}

	// Get
	rec = api.do(http.MethodGet, "/api/v1/users/"+created.ID, "")
	expectStatus(t, rec, http.StatusOK)

	// Patch
	rec = api.do(http.MethodPatch, "/api/v1/users/"+created.ID, `{"name":"Lan Pham"}`)
	expectStatus(t, rec, http.StatusOK)
	if got := decode[user](t, rec); got.Name != "Lan Pham" || got.Email != "lan@example.com" {
		t.Fatalf("patch result %+v", got)
	}

	// List
	api.do(http.MethodPost, "/api/v1/users", `{"email":"minh@example.com","name":"Minh"}`)
	rec = api.do(http.MethodGet, "/api/v1/users?limit=1&offset=1", "")
	expectStatus(t, rec, http.StatusOK)
	list := decode[struct {
		Data []user `json:"data"`
		Meta struct{ Limit, Offset, Total int }
	}](t, rec)
	if list.Meta.Total != 2 || len(list.Data) != 1 || list.Data[0].Email != "minh@example.com" {
		t.Fatalf("list %+v", list)
	}

	// Delete
	expectStatus(t, api.do(http.MethodDelete, "/api/v1/users/"+created.ID, ""), http.StatusNoContent)
	expectStatus(t, api.do(http.MethodGet, "/api/v1/users/"+created.ID, ""), http.StatusNotFound)
}

func TestErrorResponses(t *testing.T) {
	api := newAPI(t, nil)
	api.do(http.MethodPost, "/api/v1/users", `{"email":"dup@example.com","name":"Dup"}`)

	tests := []struct {
		name, method, path, body string
		wantStatus               int
		wantFields               int
	}{
		{"validation", http.MethodPost, "/api/v1/users", `{"email":"x","name":""}`, http.StatusUnprocessableEntity, 2},
		{"duplicate email", http.MethodPost, "/api/v1/users", `{"email":"DUP@example.com","name":"Again"}`, http.StatusConflict, 0},
		{"malformed json", http.MethodPost, "/api/v1/users", `{"email":`, http.StatusBadRequest, 0},
		{"unknown id", http.MethodGet, "/api/v1/users/0192f3a4-5b6c-7d8e-9f01-23456789abcd", "", http.StatusNotFound, 0},
		{"garbage id", http.MethodGet, "/api/v1/users/not-a-uuid", "", http.StatusNotFound, 0},
		{"bad limit", http.MethodGet, "/api/v1/users?limit=abc", "", http.StatusBadRequest, 0},
		{"offset too deep", http.MethodGet, "/api/v1/users?offset=10001", "", http.StatusUnprocessableEntity, 1},
		{"unknown route", http.MethodGet, "/nope", "", http.StatusNotFound, 0},
		{"method not allowed", http.MethodPut, "/api/v1/users", `{}`, http.StatusMethodNotAllowed, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := api.do(tc.method, tc.path, tc.body)
			expectStatus(t, rec, tc.wantStatus)
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
				t.Fatalf("Content-Type = %q", ct)
			}
			p := decode[problem](t, rec)
			if p.Status != tc.wantStatus || p.Title == "" {
				t.Fatalf("problem body %+v", p)
			}
			if len(p.Errors) != tc.wantFields {
				t.Fatalf("errors = %+v, want %d", p.Errors, tc.wantFields)
			}
		})
	}
}

func TestHealthAndReadiness(t *testing.T) {
	expectStatus(t, newAPI(t, nil).do(http.MethodGet, "/healthz", ""), http.StatusOK)
	expectStatus(t, newAPI(t, nil).do(http.MethodGet, "/readyz", ""), http.StatusOK)

	down := newAPI(t, func(context.Context) error { return errors.New("db down") })
	expectStatus(t, down.do(http.MethodGet, "/readyz", ""), http.StatusServiceUnavailable)
}

func TestClientCanceledRequest_Is499AndNotLoggedAsError(t *testing.T) {
	var logs bytes.Buffer
	api := newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // client đã đóng kết nối
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	api.h.ServeHTTP(rec, req)

	expectStatus(t, rec, httpapi.StatusClientClosedRequest)
	if strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("client cancellation must not be logged as ERROR: %s", logs.String())
	}
}

func TestProbesAreNotLogged(t *testing.T) {
	var logs bytes.Buffer
	api := newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	api.do(http.MethodGet, "/healthz", "")
	api.do(http.MethodGet, "/readyz", "")
	if logs.Len() != 0 {
		t.Fatalf("probe requests were logged: %s", logs.String())
	}
	api.do(http.MethodGet, "/api/v1/users", "")
	if !strings.Contains(logs.String(), "http_request") {
		t.Fatal("API requests must still be logged")
	}
}

func TestClientIP_TrustedProxies(t *testing.T) {
	_, lb, _ := net.ParseCIDR("10.0.0.0/8")
	tests := []struct {
		name    string
		trusted []*net.IPNet
		remote  string
		xff     string
		want    string
	}{
		{"no proxy config ignores XFF", nil, "10.1.2.3:1234", "203.0.113.7", "10.1.2.3"},
		{"trusted LB uses XFF", []*net.IPNet{lb}, "10.1.2.3:1234", "203.0.113.7", "203.0.113.7"},
		{"spoofed hop before client is ignored", []*net.IPNet{lb}, "10.1.2.3:1234", "1.1.1.1, 203.0.113.7", "203.0.113.7"},
		{"untrusted peer cannot spoof", []*net.IPNet{lb}, "192.168.5.5:1234", "203.0.113.7", "192.168.5.5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			api := newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil)), TrustedProxies: tc.trusted})
			req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
			req.RemoteAddr = tc.remote
			req.Header.Set("X-Forwarded-For", tc.xff)
			api.h.ServeHTTP(httptest.NewRecorder(), req)
			if want := `"remote_ip":"` + tc.want + `"`; !strings.Contains(logs.String(), want) {
				t.Fatalf("want %s in log, got %s", want, logs.String())
			}
		})
	}
}

func TestAPIMiddleware_AppliesToAPIOnly(t *testing.T) {
	deny := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { return echo.ErrUnauthorized }
	}
	api := newAPIWith(t, httpapi.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIMiddleware: []echo.MiddlewareFunc{deny},
	})
	expectStatus(t, api.do(http.MethodGet, "/api/v1/users", ""), http.StatusUnauthorized)
	expectStatus(t, api.do(http.MethodGet, "/healthz", ""), http.StatusOK)
}
