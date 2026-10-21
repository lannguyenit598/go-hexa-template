package httpapi_test

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
)

// The contract tests replay request sequences against a real server and compare every status,
// response header (except Date), and body byte for byte with testdata/*.golden. Regenerate with
// `go test ./internal/adapter/inbound/httpapi -run TestContract -update` only for intended
// contract changes, and review the diff like any API change.
var update = flag.Bool("update", false, "rewrite the contract golden files")

type exchange struct {
	name    string
	method  string
	path    string
	header  []string // Name/value pairs; repeated names send repeated header lines.
	body    string
	chunked bool // Send the body without Content-Length.
	at      string
}

type contractClock struct{ now time.Time }

func (c *contractClock) Now() time.Time { return c.now }

type contractIDs struct{ n int }

func (g *contractIDs) NewID() domain.UserID {
	g.n++
	return domain.UserID(fmt.Sprintf("01a15359-aa28-7a00-a380-%012d", g.n))
}

func TestContract_UserAPI(t *testing.T) {
	clock := &contractClock{}
	svc := service.NewUserService(memory.NewUserRepository(), &contractIDs{}, clock, nil)
	h := httpapi.NewServer(svc, httpapi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	const (
		id1     = "/api/v1/users/01a15359-aa28-7a00-a380-000000000001"
		id2     = "/api/v1/users/01a15359-aa28-7a00-a380-000000000002"
		missing = "/api/v1/users/01a15359-aa28-7a00-a380-000000000999"
	)
	json := []string{"Content-Type", "application/json"}
	big := `{"email":"big@example.com","name":"` + strings.Repeat("a", 1<<20) + `"}`

	runContract(t, h, clock, "user_api.golden", []exchange{
		{name: "list is empty", method: "GET", path: "/api/v1/users"},
		{name: "create normalizes email and name", method: "POST", path: "/api/v1/users", header: json,
			body: `{"email":" Lan@Example.COM ","name":"  Lan \t  Nguyen ","role":"ignored"}`, at: "2026-10-19T08:49:07.624656Z"},
		{name: "create escapes HTML characters", method: "POST", path: "/api/v1/users", header: json,
			body: `{"email":"minh@example.com","name":"<b>Minh</b> & co"}`, at: "2026-10-19T08:49:07.5Z"},
		{name: "create duplicate email", method: "POST", path: "/api/v1/users", header: json, body: `{"email":"LAN@example.com","name":"Again"}`},
		{name: "create validation", method: "POST", path: "/api/v1/users", header: json, body: `{"email":"x","name":""}`},
		{name: "create length limits count code points", method: "POST", path: "/api/v1/users", header: json,
			body: `{"email":"` + strings.Repeat("a", 245) + `@example.com","name":"` + strings.Repeat("ế", 101) + `"}`},
		{name: "create name of 100 multibyte code points", method: "POST", path: "/api/v1/users", header: json,
			body: `{"email":"long@example.com","name":"` + strings.Repeat("ế", 100) + `"}`},
		{name: "create empty body", method: "POST", path: "/api/v1/users"},
		{name: "create empty chunked body", method: "POST", path: "/api/v1/users", header: json, chunked: true},
		{name: "create empty object", method: "POST", path: "/api/v1/users", header: json, body: `{}`},
		{name: "create null body", method: "POST", path: "/api/v1/users", header: json, body: `null`},
		{name: "create malformed JSON", method: "POST", path: "/api/v1/users", header: json, body: `{"email":`},
		{name: "create wrong field type", method: "POST", path: "/api/v1/users", header: json, body: `{"email":5,"name":"X"}`},
		{name: "create non-object body", method: "POST", path: "/api/v1/users", header: json, body: `["x"]`},
		{name: "create trailing data", method: "POST", path: "/api/v1/users", header: json, body: `{"email":"t@example.com","name":"T"} {}`},
		{name: "create without Content-Type", method: "POST", path: "/api/v1/users", body: `{"email":"n@example.com","name":"N"}`},
		{name: "create text/plain", method: "POST", path: "/api/v1/users", header: []string{"Content-Type", "text/plain"}, body: `{"email":"n@example.com","name":"N"}`},
		{name: "create merge-patch media type", method: "POST", path: "/api/v1/users", header: []string{"Content-Type", "application/merge-patch+json"}, body: `{}`},
		{name: "create case-insensitive media type with parameter", method: "POST", path: "/api/v1/users",
			header: []string{"Content-Type", "APPLICATION/JSON;charset=UTF-8"}, body: `{"email":"case@example.com","name":"Case"}`},
		{name: "create body over 1 MiB", method: "POST", path: "/api/v1/users", header: json, body: big},
		{name: "create chunked body over 1 MiB", method: "POST", path: "/api/v1/users", header: json, body: big, chunked: true},
		{name: "create with Accept CBOR", method: "POST", path: "/api/v1/users",
			header: []string{"Content-Type", "application/json", "Accept", "application/cbor"}, body: `{"email":"cbor@example.com","name":"Cbor"}`},

		{name: "list", method: "GET", path: "/api/v1/users"},
		{name: "list with Accept text/html", method: "GET", path: "/api/v1/users?limit=1", header: []string{"Accept", "text/html"}},
		{name: "list with Accept json", method: "GET", path: "/api/v1/users?limit=1", header: []string{"Accept", "json"}},
		{name: "list with Accept any", method: "GET", path: "/api/v1/users?limit=1", header: []string{"Accept", "*/*"}},
		{name: "list with Accept vendor JSON", method: "GET", path: "/api/v1/users?limit=1", header: []string{"Accept", "application/vnd.api+json"}},
		{name: "list with Accept problem JSON", method: "GET", path: "/api/v1/users?limit=1", header: []string{"Accept", "application/problem+json"}},
		{name: "get missing with Accept json", method: "GET", path: missing, header: []string{"Accept", "json"}},
		{name: "list limit zero uses default", method: "GET", path: "/api/v1/users?limit=0"},
		{name: "list negative limit uses default", method: "GET", path: "/api/v1/users?limit=-3"},
		{name: "list limit above 100 is capped", method: "GET", path: "/api/v1/users?limit=1000"},
		{name: "list page", method: "GET", path: "/api/v1/users?limit=1&offset=1"},
		{name: "list signed limit", method: "GET", path: "/api/v1/users?limit=+2"},
		{name: "list empty limit", method: "GET", path: "/api/v1/users?limit="},
		{name: "list first of repeated limits", method: "GET", path: "/api/v1/users?limit=1&limit=abc"},
		{name: "list non-integer limit", method: "GET", path: "/api/v1/users?limit=abc"},
		{name: "list fractional limit", method: "GET", path: "/api/v1/users?limit=1.5"},
		{name: "list non-integer offset", method: "GET", path: "/api/v1/users?offset=x"},
		{name: "list invalid limit and offset", method: "GET", path: "/api/v1/users?limit=x&offset=y"},
		{name: "list negative offset uses zero", method: "GET", path: "/api/v1/users?offset=-5"},
		{name: "list offset at maximum", method: "GET", path: "/api/v1/users?offset=10000"},
		{name: "list offset above maximum", method: "GET", path: "/api/v1/users?offset=10001"},
		{name: "list ignores unknown query", method: "GET", path: "/api/v1/users?limit=1&sort=name"},

		{name: "get", method: "GET", path: id1},
		{name: "get missing", method: "GET", path: missing},
		{name: "get malformed id", method: "GET", path: "/api/v1/users/not-a-uuid"},
		{name: "get uppercase id under users", method: "GET", path: "/api/v1/users/01A15359-AA28-7A00-A380-000000000001"},
		{name: "get UUID v4", method: "GET", path: "/api/v1/users/0b1c2d3e-4f50-4a6b-8c7d-9e0f1a2b3c4d"},
		{name: "get percent-encoded id", method: "GET", path: "/api/v1/users/01a15359%2Daa28-7a00-a380-000000000001"},

		{name: "patch empty body is a no-op", method: "PATCH", path: id1, at: "2026-10-19T09:00:00Z"},
		{name: "patch empty chunked body without Content-Type", method: "PATCH", path: id1, chunked: true},
		{name: "patch empty object is a no-op", method: "PATCH", path: id1, header: json, body: `{}`},
		{name: "patch null fields are unchanged", method: "PATCH", path: id1, header: json, body: `{"email":null,"name":null}`},
		{name: "patch same values is a no-op", method: "PATCH", path: id1, header: json, body: `{"name":"Lan Nguyen"}`},
		{name: "patch with matching If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `"1"`},
			body: `{"name":" Lan  Pham "}`, at: "2026-10-19T09:00:00.5722Z"},
		{name: "patch with stale If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `"1"`}, body: `{"name":"Lost"}`},
		{name: "patch with weak If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `W/"2"`}, body: `{"name":"Lost"}`},
		{name: "patch with signed If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `"+2"`}, body: `{"name":"Lost"}`},
		{name: "patch with leading-zero If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `"02"`}, body: `{"name":"Lost"}`},
		{name: "patch with empty If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", ``}, body: `{"name":"Lost"}`},
		{name: "patch with If-Match list", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `"2", "3"`}, body: `{"name":"Lost"}`},
		{name: "patch with repeated If-Match lines", method: "PATCH", path: id1,
			header: []string{"Content-Type", "application/json", "If-Match", `"2"`, "If-Match", `"3"`}, body: `{"name":"Lost"}`},
		{name: "patch with invalid If-Match and invalid media type", method: "PATCH", path: id1,
			header: []string{"Content-Type", "text/plain", "If-Match", `W/"2"`}, body: `{"name":"Lost"}`},
		{name: "patch with If-Match on missing user", method: "PATCH", path: missing, header: []string{"Content-Type", "application/json", "If-Match", `"1"`}, body: `{"name":"X"}`},
		{name: "patch with wildcard If-Match", method: "PATCH", path: id1, header: []string{"Content-Type", "application/json", "If-Match", `*`}, body: `{"email":"LAN.PHAM@example.com"}`},
		{name: "patch duplicate email", method: "PATCH", path: id1, header: json, body: `{"email":"minh@example.com"}`},
		{name: "patch validation", method: "PATCH", path: id1, header: json, body: `{"email":"","name":"   "}`},
		{name: "patch malformed JSON", method: "PATCH", path: id1, header: json, body: `{`},
		{name: "patch wrong field type", method: "PATCH", path: id1, header: json, body: `{"name":1}`},
		{name: "patch without Content-Type", method: "PATCH", path: id1, body: `{"name":"X"}`},
		{name: "patch missing user", method: "PATCH", path: missing, header: json, body: `{"name":"X"}`},
		{name: "patch malformed id", method: "PATCH", path: "/api/v1/users/nope", header: json, body: `{"name":"X"}`},
		{name: "patch body over 1 MiB", method: "PATCH", path: id1, header: json, body: big, chunked: true},

		{name: "delete ignores If-Match", method: "DELETE", path: id2, header: []string{"If-Match", `W/"garbage"`}},
		{name: "delete again", method: "DELETE", path: id2},
		{name: "delete malformed id", method: "DELETE", path: "/api/v1/users/nope"},
		{name: "delete with body", method: "DELETE", path: id1, header: []string{"Content-Type", "text/plain"}, body: "x"},
		{name: "list after changes", method: "GET", path: "/api/v1/users"},

		{name: "unknown route", method: "GET", path: "/nope"},
		{name: "unknown API route", method: "GET", path: "/api/v1/nope"},
		{name: "API root", method: "GET", path: "/api/v1"},
		{name: "collection trailing slash", method: "GET", path: "/api/v1/users/"},
		{name: "collection trailing slash POST", method: "POST", path: "/api/v1/users/", header: json, body: `{}`},
		{name: "item trailing slash", method: "GET", path: id1 + "/"},
		{name: "collection wrong method", method: "PUT", path: "/api/v1/users", header: json, body: `{}`},
		{name: "item wrong method", method: "POST", path: id1, header: json, body: `{}`},
		{name: "collection HEAD", method: "HEAD", path: "/api/v1/users"},
		{name: "item HEAD", method: "HEAD", path: id1},
		{name: "collection OPTIONS", method: "OPTIONS", path: "/api/v1/users"},
		{name: "OpenAPI is not exposed", method: "GET", path: "/openapi.json"},
		{name: "OpenAPI YAML is not exposed", method: "GET", path: "/openapi.yaml"},
		{name: "docs are not exposed", method: "GET", path: "/docs"},
		{name: "schemas are not exposed", method: "GET", path: "/schemas/User.json"},
		{name: "liveness", method: "GET", path: "/healthz"},
		{name: "liveness HEAD", method: "HEAD", path: "/healthz"},
		{name: "readiness", method: "GET", path: "/readyz"},
	})
}

// failingUsers fails every use case with err, or panics when err is nil.
type failingUsers struct{ err error }

func (f failingUsers) fail() error {
	if f.err == nil {
		panic("boom")
	}
	return f.err
}

func (f failingUsers) Register(context.Context, port.RegisterUserCmd) (domain.User, error) {
	return domain.User{}, f.fail()
}

func (f failingUsers) Get(context.Context, string) (domain.User, error) {
	return domain.User{}, f.fail()
}

func (f failingUsers) List(context.Context, port.ListUsersQuery) (port.UserPage, error) {
	return port.UserPage{}, f.fail()
}

func (f failingUsers) Update(context.Context, port.UpdateUserCmd) (domain.User, error) {
	return domain.User{}, f.fail()
}
func (f failingUsers) Delete(context.Context, string) error { return f.fail() }

// waitingUsers blocks until the request context ends, as a slow database would.
type waitingUsers struct{ failingUsers }

func (waitingUsers) Get(ctx context.Context, _ string) (domain.User, error) {
	<-ctx.Done()
	return domain.User{}, fmt.Errorf("get user: %w", ctx.Err())
}

func TestContract_ServiceFailures(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	id := "/api/v1/users/01a15359-aa28-7a00-a380-000000000001"
	cases := []struct {
		file  string
		users port.UserService
		opts  httpapi.Options
	}{
		{"unexpected_error.golden", failingUsers{errors.New("dial tcp 10.0.0.5:5432: password=secret")}, httpapi.Options{Logger: logger}},
		{"version_conflict.golden", failingUsers{fmt.Errorf("update user: %w", port.ErrVersionConflict)}, httpapi.Options{Logger: logger}},
		{"panic.golden", failingUsers{}, httpapi.Options{Logger: logger}},
		{"timeout.golden", waitingUsers{}, httpapi.Options{Logger: logger, RequestTimeout: 10 * time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			runContract(t, httpapi.NewServer(tc.users, tc.opts), &contractClock{}, tc.file, []exchange{
				{name: "create", method: "POST", path: "/api/v1/users", header: []string{"Content-Type", "application/json"}, body: `{"email":"a@example.com","name":"A"}`},
				{name: "list", method: "GET", path: "/api/v1/users"},
				{name: "get", method: "GET", path: id},
				{name: "get HEAD", method: "HEAD", path: id},
				{name: "update", method: "PATCH", path: id, header: []string{"Content-Type", "application/json"}, body: `{"name":"B"}`},
				{name: "update with If-Match", method: "PATCH", path: id, header: []string{"Content-Type", "application/json", "If-Match", `"1"`}, body: `{"name":"B"}`},
				{name: "delete", method: "DELETE", path: id},
			})
		})
	}
}

func runContract(t *testing.T, h http.Handler, clock *contractClock, file string, exchanges []exchange) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}

	var out strings.Builder
	for i, ex := range exchanges {
		if ex.at != "" {
			at, err := time.Parse(time.RFC3339Nano, ex.at)
			if err != nil {
				t.Fatal(err)
			}
			clock.now = at
		}
		var body io.Reader
		if ex.body != "" || ex.chunked {
			body = strings.NewReader(ex.body)
			if ex.chunked {
				body = io.MultiReader(body) // Hide the length so the client sends a chunked body.
			}
		}
		req, err := http.NewRequestWithContext(context.Background(), ex.method, srv.URL+ex.path, body)
		if err != nil {
			t.Fatal(err)
		}
		if ex.chunked {
			req.ContentLength = -1
		}
		req.Header.Set("X-Request-Id", fmt.Sprintf("contract-%02d", i))
		for j := 0; j+1 < len(ex.header); j += 2 {
			req.Header.Add(ex.header[j], ex.header[j+1])
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", ex.name, err)
		}
		respBody, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close() // skip: the body was fully read.
		if err != nil {
			t.Fatalf("%s: read body: %v", ex.name, err)
		}

		fmt.Fprintf(&out, "=== %s\n%s %s\n", ex.name, ex.method, ex.path)
		writeHeaders(&out, req.Header)
		fmt.Fprintf(&out, "\n%s\n--- %s\n", abbreviate(ex.body), resp.Status)
		resp.Header.Del("Date")
		writeHeaders(&out, resp.Header)
		fmt.Fprintf(&out, "\n%s\n\n", respBody)
	}

	path := filepath.Join("testdata", file)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := fs.ReadFile(os.DirFS("testdata"), file)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got := out.String(); got != string(want) {
		t.Errorf("contract changed; diff %s against the output below, or run with -update for an intended change:\n%s", path, firstDiff(string(want), got))
	}
}

func writeHeaders(out *strings.Builder, h http.Header) {
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		for _, v := range h[name] {
			fmt.Fprintf(out, "%s: %s\n", name, v)
		}
	}
}

func abbreviate(s string) string {
	if len(s) > 300 {
		return fmt.Sprintf("<%d bytes>", len(s))
	}
	return s
}

// firstDiff shows the first differing exchange of two golden outputs.
func firstDiff(want, got string) string {
	w, g := strings.Split(want, "\n=== "), strings.Split(got, "\n=== ")
	for i := range min(len(w), len(g)) {
		if w[i] != g[i] {
			return "want:\n=== " + w[i] + "\ngot:\n=== " + g[i]
		}
	}
	return fmt.Sprintf("want %d exchanges, got %d", len(w), len(g))
}
