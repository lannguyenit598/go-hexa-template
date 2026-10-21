package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	svc := service.NewUserService(memory.NewUserRepository(), system.UUIDv7{}, system.Clock{}, nil)
	return &apiClient{t: t, h: httpapi.NewServer(svc, opts)}
}

func (a *apiClient) do(method, path, body string) *httptest.ResponseRecorder {
	a.t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, r)
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
	Version         int64
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
		{"patch garbage id", http.MethodPatch, "/api/v1/users/not-a-uuid", `{"name":"x"}`, http.StatusNotFound, 0},
		{"delete garbage id", http.MethodDelete, "/api/v1/users/not-a-uuid", "", http.StatusNotFound, 0},
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
	cancel() // The client closed the connection.
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/users", nil)
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
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/users", nil)
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

func (a *apiClient) doWithHeader(method, path, body, key, value string) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(key, value)
	rec := httptest.NewRecorder()
	a.h.ServeHTTP(rec, req)
	return rec
}

func TestETagAndIfMatch(t *testing.T) {
	api := newAPI(t, nil)
	rec := api.do(http.MethodPost, "/api/v1/users", `{"email":"e@example.com","name":"E"}`)
	expectStatus(t, rec, http.StatusCreated)
	created := decode[user](t, rec)
	if rec.Header().Get("ETag") != `"1"` || created.Version != 1 {
		t.Fatalf("ETag = %q, version = %d", rec.Header().Get("ETag"), created.Version)
	}
	path := "/api/v1/users/" + created.ID

	if got := api.do(http.MethodGet, path, "").Header().Get("ETag"); got != `"1"` {
		t.Fatalf("GET ETag = %q", got)
	}

	// Matching version: success with a new ETag.
	rec = api.doWithHeader(http.MethodPatch, path, `{"name":"E2"}`, "If-Match", `"1"`)
	expectStatus(t, rec, http.StatusOK)
	if rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("ETag after update = %q", rec.Header().Get("ETag"))
	}

	// Stale versions, weak ETags, and malformed values: 412 without writes.
	// "+2" and "02" have the current numeric version but differ from the issued ETag:
	// RFC 9110 strong comparison matches character for character.
	for _, ifMatch := range []string{`"1"`, `W/"2"`, `garbage`, `"+2"`, `"02"`, `" 2"`} {
		rec = api.doWithHeader(http.MethodPatch, path, `{"name":"Lost"}`, "If-Match", ifMatch)
		expectStatus(t, rec, http.StatusPreconditionFailed)
		if p := decode[problem](t, rec); p.Status != http.StatusPreconditionFailed {
			t.Fatalf("problem %+v", p)
		}
	}
	if got := decode[user](t, api.do(http.MethodGet, path, "")); got.Name != "E2" || got.Version != 2 {
		t.Fatalf("rejected update must not be applied: %+v", got)
	}

	// Empty headers and multiple ETags (even including the current version) return 412 rather than
	// being treated as absent If-Match and permitting unconditional overwrites.
	for _, ifMatch := range []string{``, `"2", "3"`} {
		rec = api.doWithHeader(http.MethodPatch, path, `{"name":"Lost"}`, "If-Match", ifMatch)
		expectStatus(t, rec, http.StatusPreconditionFailed)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPatch, path, strings.NewReader(`{"name":"Lost"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Add("If-Match", `"2"`)
	req.Header.Add("If-Match", `"3"`)
	rec = httptest.NewRecorder()
	api.h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusPreconditionFailed)
	if got := decode[user](t, api.do(http.MethodGet, path, "")); got.Name != "E2" || got.Version != 2 {
		t.Fatalf("rejected update must not be applied: %+v", got)
	}

	// "*" and absent If-Match: no version precondition.
	expectStatus(t, api.doWithHeader(http.MethodPatch, path, `{"name":"E3"}`, "If-Match", "*"), http.StatusOK)
	expectStatus(t, api.do(http.MethodPatch, path, `{"name":"E4"}`), http.StatusOK)
}

// Only JSON bodies are accepted: Echo BindBody also accepts forms/XML, silently ignoring fields (confusing 422).
func TestRequestBody_OnlyJSONIsAccepted(t *testing.T) {
	api := newAPI(t, nil)
	rec := api.do(http.MethodPost, "/api/v1/users", `{"email":"ct@example.com","name":"CT"}`)
	expectStatus(t, rec, http.StatusCreated)
	userPath := "/api/v1/users/" + decode[user](t, rec).ID

	contentTypes := map[string]string{
		"missing":       "",
		"form":          "application/x-www-form-urlencoded",
		"multipart":     "multipart/form-data; boundary=x",
		"xml":           "application/xml",
		"text":          "text/plain",
		"merge-patch":   "application/merge-patch+json",
		"json-ish typo": "application/jsonx",
	}
	for name, ct := range contentTypes {
		for _, tc := range []struct{ method, path, body string }{
			{http.MethodPost, "/api/v1/users", "email=form@example.com&name=Form"},
			{http.MethodPatch, userPath, `{"name":"Form"}`},
		} {
			t.Run(name+" "+tc.method, func(t *testing.T) {
				rec := api.doWithHeader(tc.method, tc.path, tc.body, "Content-Type", ct)
				expectStatus(t, rec, http.StatusUnsupportedMediaType)
			})
		}
	}
	// Media types are case-insensitive (RFC 9110); parameters such as charset are accepted.
	for i, ct := range []string{"application/json; charset=utf-8", "Application/JSON", "APPLICATION/JSON;charset=UTF-8"} {
		body := fmt.Sprintf(`{"email":"cs%d@example.com","name":"CS"}`, i)
		rec = api.doWithHeader(http.MethodPost, "/api/v1/users", body, "Content-Type", ct)
		expectStatus(t, rec, http.StatusCreated)
	}
}

func TestRequestBody_BadRequestDetail(t *testing.T) {
	api := newAPI(t, nil)
	tests := []struct{ name, body, detail string }{
		{"syntax error", `{"email":`, "request body is not valid JSON"},
		{"wrong field type", `{"email":5,"name":"X"}`, `field "email" has the wrong type`},
		{"not an object", `["x"]`, "request body must be a JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := api.do(http.MethodPost, "/api/v1/users", tc.body)
			expectStatus(t, rec, http.StatusBadRequest)
			if p := decode[problem](t, rec); p.Detail != tc.detail {
				t.Fatalf("detail = %q, want %q", p.Detail, tc.detail)
			}
		})
	}
}

// An oversized chunked body (no Content-Length) must still return 413, not 400.
func TestRequestBody_TooLargeWithoutContentLength(t *testing.T) {
	api := newAPI(t, nil)
	body := `{"email":"big@example.com","name":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/users", io.NopCloser(strings.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	api.h.ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusRequestEntityTooLarge)
}

func TestRequestID_ClientValueIsSanitized(t *testing.T) {
	api := newAPI(t, nil)
	cases := map[string]bool{ // Submitted header -> whether it should be preserved.
		"0b1c2d3e-trace.id_1":        true,
		strings.Repeat("a", 129):     false,
		"evil\"} injected {\"x\":\"": false,
		"has space":                  false,
	}
	for in, keep := range cases {
		got := api.doWithHeader(http.MethodGet, "/api/v1/users", "", "X-Request-Id", in).Header().Get("X-Request-Id")
		if keep && got != in {
			t.Errorf("valid id %q replaced by %q", in, got)
		}
		if !keep && (got == in || got == "") {
			t.Errorf("invalid id %q not replaced (got %q)", in, got)
		}
	}
}

func TestAPIMiddleware_RunsOncePerAPIRequest(t *testing.T) {
	calls := 0
	count := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error { calls++; return next(c) }
	}
	api := newAPIWith(t, httpapi.Options{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		APIMiddleware: []echo.MiddlewareFunc{count},
	})
	created := decode[user](t, api.do(http.MethodPost, "/api/v1/users", `{"email":"mw@example.com","name":"MW"}`))
	id := "/api/v1/users/" + created.ID
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/api/v1/users", `{"email":"mw2@example.com","name":"MW"}`, 1},
		{http.MethodGet, "/api/v1/users", "", 1},
		{http.MethodGet, id, "", 1},
		{http.MethodPatch, id, `{"name":"MW2"}`, 1},
		{http.MethodDelete, id, "", 1},
		{http.MethodGet, "/api/v1/nope", "", 1},
		{http.MethodPut, "/api/v1/users", `{}`, 1},
		{http.MethodGet, "/healthz", "", 0},
		{http.MethodGet, "/readyz", "", 0},
	} {
		calls = 0
		api.do(tc.method, tc.path, tc.body)
		if calls != tc.want {
			t.Errorf("%s %s: APIMiddleware ran %d times, want %d", tc.method, tc.path, calls, tc.want)
		}
	}
}

func expectNameProblem(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	expectStatus(t, rec, http.StatusUnprocessableEntity)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if p := decode[problem](t, rec); len(p.Errors) != 1 || p.Errors[0].Field != "name" {
		t.Fatalf("errors = %+v, want one name error", p.Errors)
	}
}

func TestCreate_RejectsNULInName(t *testing.T) {
	api := newAPI(t, nil)
	expectNameProblem(t, api.do(http.MethodPost, "/api/v1/users", `{"email":"nul@example.com","name":"a\u0000b"}`))
}

// A PATCH rejected for its name changes nothing, including the valid email in the same body.
func TestUpdate_RejectsNULInName(t *testing.T) {
	api := newAPI(t, nil)
	rec := api.do(http.MethodPost, "/api/v1/users", `{"email":"old@example.com","name":"Old"}`)
	expectStatus(t, rec, http.StatusCreated)
	created := decode[user](t, rec)
	expectNameProblem(t, api.do(http.MethodPatch, "/api/v1/users/"+created.ID, `{"email":"new@example.com","name":"a\u0000b"}`))
	rec = api.do(http.MethodGet, "/api/v1/users/"+created.ID, "")
	expectStatus(t, rec, http.StatusOK)
	if got := decode[user](t, rec); got.Version != 1 || got.Email != "old@example.com" {
		t.Fatalf("after rejected PATCH: %+v, want version 1 and the old email", got)
	}
}
