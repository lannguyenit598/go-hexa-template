package httpapi_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/adapter/inbound/httpapi"
)

// Minimal OpenAPI 3.1 model: only what the contract assertions read.
type openAPIDoc struct {
	OpenAPI    string                          `json:"openapi"`
	Info       struct{ Title, Version string } `json:"info"`
	Paths      map[string]map[string]openAPIOp `json:"paths"`
	Security   []map[string][]string           `json:"security"`
	Components struct {
		Schemas         map[string]openAPISchema `json:"schemas"`
		SecuritySchemes map[string]any           `json:"securitySchemes"`
	} `json:"components"`
}

type openAPIOp struct {
	OperationID string                `json:"operationId"`
	Tags        []string              `json:"tags"`
	Security    []map[string][]string `json:"security"`
	Parameters  []struct {
		Name     string        `json:"name"`
		In       string        `json:"in"`
		Required bool          `json:"required"`
		Schema   openAPISchema `json:"schema"`
	} `json:"parameters"`
	RequestBody *struct {
		Required bool                                      `json:"required"`
		Content  map[string]struct{ Schema openAPISchema } `json:"content"`
	} `json:"requestBody"`
	Responses map[string]struct {
		Headers map[string]any                            `json:"headers"`
		Content map[string]struct{ Schema openAPISchema } `json:"content"`
	} `json:"responses"`
}

type openAPISchema struct {
	Ref                  string                   `json:"$ref"`
	Type                 any                      `json:"type"`
	Format               string                   `json:"format"`
	Required             []string                 `json:"required"`
	Properties           map[string]openAPISchema `json:"properties"`
	AdditionalProperties any                      `json:"additionalProperties"`
	Items                *openAPISchema           `json:"items"`
	MinLength            *int                     `json:"minLength"`
	MaxLength            *int                     `json:"maxLength"`
	Minimum              *float64                 `json:"minimum"`
	Maximum              *float64                 `json:"maximum"`
}

func newDocsAPI(t *testing.T, mw ...echo.MiddlewareFunc) *apiClient {
	t.Helper()
	return newAPIWith(t, httpapi.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), APIDocs: true, APIMiddleware: mw})
}

func fetchSpec(t *testing.T, api *apiClient) openAPIDoc {
	t.Helper()
	rec := api.do(http.MethodGet, "/openapi.json", "")
	expectStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); ct != "application/openapi+json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	return decode[openAPIDoc](t, rec)
}

func TestAPIDocs_EnabledServesSpecAndDocsOutsideAPIMiddleware(t *testing.T) {
	deny := func(echo.HandlerFunc) echo.HandlerFunc {
		return func(*echo.Context) error { return echo.ErrUnauthorized }
	}
	api := newDocsAPI(t, deny)
	for path, contentType := range map[string]string{
		"/openapi.json": "application/openapi+json",
		"/docs":         "text/html",
	} {
		rec := api.do(http.MethodGet, path, "")
		expectStatus(t, rec, http.StatusOK)
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, contentType) {
			t.Errorf("%s: Content-Type = %q, want %s", path, ct, contentType)
		}
	}
	if csp := api.do(http.MethodGet, "/docs", "").Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("docs Content-Security-Policy = %q", csp)
	}
	for _, path := range []string{"/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml", "/schemas/UserResponse.json"} {
		expectStatus(t, api.do(http.MethodGet, path, ""), http.StatusNotFound)
	}
	if body := api.do(http.MethodGet, "/docs", "").Body.String(); !strings.Contains(body, `"/openapi.json"`) {
		t.Errorf("docs page must load /openapi.json: %s", body)
	}
	expectStatus(t, api.do(http.MethodGet, "/api/v1/users", ""), http.StatusUnauthorized)
}

func TestAPIDocs_SpecDescribesTheUserAPI(t *testing.T) {
	spec := fetchSpec(t, newDocsAPI(t))
	if spec.OpenAPI != "3.1.0" || spec.Info.Title == "" || spec.Info.Version == "" {
		t.Fatalf("openapi %q, info %+v", spec.OpenAPI, spec.Info)
	}

	ops := map[string]openAPIOp{}
	for path, methods := range spec.Paths {
		for method, op := range methods {
			ops[strings.ToUpper(method)+" "+path] = op
		}
	}
	wantOps := map[string]struct {
		id      string
		success string
		headers []string
	}{
		"POST /api/v1/users":        {"createUser", "201", []string{"ETag", "Location"}},
		"GET /api/v1/users":         {"listUsers", "200", nil},
		"GET /api/v1/users/{id}":    {"getUser", "200", []string{"ETag"}},
		"PATCH /api/v1/users/{id}":  {"updateUser", "200", []string{"ETag"}},
		"DELETE /api/v1/users/{id}": {"deleteUser", "204", nil},
	}
	if got := slices.Sorted(maps.Keys(ops)); !slices.Equal(got, slices.Sorted(maps.Keys(wantOps))) {
		t.Fatalf("operations = %v", got)
	}
	for key, want := range wantOps {
		op := ops[key]
		if op.OperationID != want.id || !slices.Equal(op.Tags, []string{"Users"}) {
			t.Errorf("%s: operationId %q, tags %v", key, op.OperationID, op.Tags)
		}
		if op.Security != nil {
			t.Errorf("%s: security %v, but the API enforces no authentication", key, op.Security)
		}
		success, ok := op.Responses[want.success]
		if !ok {
			t.Errorf("%s: no %s response: %v", key, want.success, slices.Sorted(maps.Keys(op.Responses)))
		}
		if got := slices.Sorted(maps.Keys(success.Headers)); !slices.Equal(got, want.headers) {
			t.Errorf("%s: %s headers %v, want %v", key, want.success, got, want.headers)
		}
		if _, hasBody := success.Content["application/json"]; hasBody != (want.success != "204") {
			t.Errorf("%s: %s content %v", key, want.success, success.Content)
		}
		for status, resp := range op.Responses {
			if status[0] != '4' && status[0] != '5' {
				continue
			}
			media, ok := resp.Content["application/problem+json"]
			if len(resp.Content) != 1 || !ok || media.Schema.Ref != "#/components/schemas/Problem" {
				t.Errorf("%s: %s must be application/problem+json Problem, got %+v", key, status, resp.Content)
			}
		}
	}

	errorsOf := func(key string) []string {
		var codes []string
		for status := range ops[key].Responses {
			if status[0] == '4' || status[0] == '5' {
				codes = append(codes, status)
			}
		}
		slices.Sort(codes)
		return codes
	}
	for key, want := range map[string][]string{
		"POST /api/v1/users":        {"400", "409", "413", "415", "422", "500", "503"},
		"GET /api/v1/users":         {"400", "422", "500", "503"},
		"GET /api/v1/users/{id}":    {"404", "500", "503"},
		"PATCH /api/v1/users/{id}":  {"400", "404", "409", "412", "413", "415", "422", "500", "503"},
		"DELETE /api/v1/users/{id}": {"404", "500", "503"},
	} {
		if got := errorsOf(key); !slices.Equal(got, want) {
			t.Errorf("%s: error responses %v, want %v", key, got, want)
		}
	}

	if spec.Security != nil || spec.Components.SecuritySchemes != nil {
		t.Errorf("security %v, schemes %v: the API enforces no authentication", spec.Security, spec.Components.SecuritySchemes)
	}
	// Huma's error model is removed from components, so nothing may still reference it: an
	// operation without problemResponses would get a "default" response pointing to it.
	raw := newDocsAPI(t).do(http.MethodGet, "/openapi.json", "").Body.String()
	for _, name := range []string{"ErrorModel", "ErrorDetail"} {
		if strings.Contains(raw, name) {
			t.Errorf("document references %s", name)
		}
	}
	for key, op := range ops {
		if _, ok := op.Responses["default"]; ok {
			t.Errorf("%s: has a default response; list error statuses with problemResponses", key)
		}
	}
}

func TestAPIDocs_SchemasAcceptWhatTheAPIAccepts(t *testing.T) {
	spec := fetchSpec(t, newDocsAPI(t))
	schemas := spec.Components.Schemas
	param := func(op openAPIOp, name string) (in string, required bool, schema openAPISchema) {
		for _, p := range op.Parameters {
			if p.Name == name {
				return p.In, p.Required, p.Schema
			}
		}
		t.Fatalf("%s: no parameter %q", op.OperationID, name)
		return "", false, openAPISchema{}
	}

	// IDs are validated by the service (malformed IDs are 404), not by the schema (422).
	for _, method := range []string{"get", "patch", "delete"} {
		in, required, s := param(spec.Paths["/api/v1/users/{id}"][method], "id")
		if in != "path" || !required || s.Type != "string" || s.Format != "" || s.MinLength != nil || s.MaxLength != nil {
			t.Errorf("%s id: in %s, required %v, schema %+v", method, in, required, s)
		}
	}
	// Pagination is normalized rather than rejected, except offset > 10000 (422).
	list := spec.Paths["/api/v1/users"]["get"]
	for _, name := range []string{"limit", "offset"} {
		in, required, s := param(list, name)
		if in != "query" || required || s.Type != "integer" || s.Minimum != nil || (name == "limit" && s.Maximum != nil) {
			t.Errorf("%s: in %s, required %v, schema %+v", name, in, required, s)
		}
	}
	if _, _, s := param(list, "offset"); s.Maximum == nil || *s.Maximum != 10000 {
		t.Errorf("offset maximum = %v, want 10000", s.Maximum)
	}
	in, required, s := param(spec.Paths["/api/v1/users/{id}"]["patch"], "If-Match")
	if in != "header" || required || s.Type != "string" {
		t.Errorf("If-Match: in %s, required %v, schema %+v", in, required, s)
	}

	// Bodies: normalization happens in the domain, so no length or format limits; unknown fields
	// are ignored; an empty body is accepted by PATCH and fails validation (422) on POST.
	body := func(method, path string) openAPISchema {
		rb := spec.Paths[path][method].RequestBody
		if rb == nil || rb.Required || len(rb.Content) != 1 {
			t.Fatalf("%s %s: request body %+v", method, path, rb)
		}
		ref := rb.Content["application/json"].Schema.Ref
		return schemas[strings.TrimPrefix(ref, "#/components/schemas/")]
	}
	create := body("post", "/api/v1/users")
	if !slices.Equal(slices.Sorted(slices.Values(create.Required)), []string{"email", "name"}) || create.AdditionalProperties == false {
		t.Errorf("create body: %+v", create)
	}
	update := body("patch", "/api/v1/users/{id}")
	if len(update.Required) != 0 || update.AdditionalProperties == false {
		t.Errorf("update body: %+v", update)
	}
	for name, s := range map[string]openAPISchema{
		"create.email": create.Properties["email"], "create.name": create.Properties["name"],
		"update.email": update.Properties["email"], "update.name": update.Properties["name"],
	} {
		if s.Format != "" || s.MinLength != nil || s.MaxLength != nil {
			t.Errorf("%s: %+v must not restrict values the domain normalizes", name, s)
		}
	}
	for _, name := range []string{"email", "name"} {
		if got, _ := json.Marshal(update.Properties[name].Type); string(got) != `["string","null"]` {
			t.Errorf("update.%s type = %s, want nullable string", name, got)
		}
	}

	// Responses: every field is always present; the list is always an array.
	user := schemas["UserResponse"]
	if got := slices.Sorted(slices.Values(user.Required)); !slices.Equal(got, []string{"created_at", "email", "id", "name", "updated_at", "version"}) {
		t.Errorf("UserResponse required = %v", got)
	}
	if user.Properties["created_at"].Format != "date-time" || user.Properties["version"].Type != "integer" {
		t.Errorf("UserResponse properties = %+v", user.Properties)
	}
	if got := schemas["ListUsersResponse"].Properties["data"].Type; got != "array" {
		t.Errorf("ListUsersResponse.data type = %v, want a non-null array", got)
	}
	for _, name := range []string{"UserResponse", "ListUsersResponse", "PageMeta", "Problem", "FieldError"} {
		if schemas[name].AdditionalProperties != false {
			t.Errorf("%s.additionalProperties = %v, want false for a response", name, schemas[name].AdditionalProperties)
		}
	}
	problem := schemas["Problem"]
	if got := slices.Sorted(slices.Values(problem.Required)); !slices.Equal(got, []string{"status", "title", "type"}) {
		t.Errorf("Problem required = %v", got)
	}
	fieldErr := problem.Properties["errors"].Items
	if fieldErr == nil || fieldErr.Ref != "#/components/schemas/FieldError" || problem.Properties["errors"].Type != "array" {
		t.Errorf("Problem.errors = %+v", problem.Properties["errors"])
	}
}

// Every documented operation is routed: none answers with Echo's route-level 404 or 405.
func TestAPIDocs_EveryDocumentedOperationIsRouted(t *testing.T) {
	api := newDocsAPI(t)
	for path, methods := range fetchSpec(t, api).Paths {
		for method := range methods {
			target := strings.ReplaceAll(path, "{id}", "01a15359-aa28-7a00-a380-000000000001")
			rec := api.do(strings.ToUpper(method), target, "{}")
			if rec.Code == http.StatusMethodNotAllowed || (rec.Code == http.StatusNotFound && decode[problem](t, rec).Detail == "") {
				t.Errorf("%s %s is documented but not routed: %d %s", method, path, rec.Code, rec.Body.String())
			}
		}
	}
}

// Independently built servers share no Huma state: each documents its own five operations,
// and disabling docs on one does not affect another.
func TestAPIDocs_ServersAreIndependent(t *testing.T) {
	first, second := newDocsAPI(t), newDocsAPI(t)
	disabled := newAPI(t, nil)
	for _, api := range []*apiClient{first, second} {
		count := 0
		for _, methods := range fetchSpec(t, api).Paths {
			count += len(methods)
		}
		if count != 5 {
			t.Errorf("want 5 operations, got %d", count)
		}
	}
	expectStatus(t, disabled.do(http.MethodGet, "/openapi.json", ""), http.StatusNotFound)
	expectStatus(t, disabled.do(http.MethodGet, "/docs", ""), http.StatusNotFound)
}

// Run with -race (make check does): the first requests for the document may arrive concurrently.
func TestAPIDocs_ConcurrentRequestsShareOneDocument(t *testing.T) {
	api := newDocsAPI(t)
	bodies := make([]string, 8)
	var wg sync.WaitGroup
	for i := range bodies {
		wg.Go(func() { bodies[i] = api.do(http.MethodGet, "/openapi.json", "").Body.String() })
	}
	wg.Wait()
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Fatal("concurrent requests returned different documents")
		}
	}
}
