package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v5"
)

// newHumaAPI builds the Huma API that registers /api/v1 operations on group and generates the
// OpenAPI 3.1 document from them. Every setting is per instance: Huma's process-global error
// constructor (huma.NewError) is never replaced, so handlers return *apiError instead.
func newHumaAPI(e *echo.Echo, group *echo.Group, prefix string) huma.API {
	cfg := huma.Config{
		OpenAPI: &huma.OpenAPI{
			Info: &huma.Info{
				Title:       "userapi",
				Version:     "1.0.0",
				Description: "User management API. The API has no authentication; see docs/operations.md.",
			},
			// Relative to the document's origin: public URLs are never derived from request headers.
			Servers: []*huma.Server{{URL: "/"}},
			Tags:    []*huma.Tag{{Name: "Users", Description: "User accounts"}},
		},
		// Only JSON: Accept headers asking for other formats still receive JSON, as before Huma.
		Formats:       map[string]huma.Format{"application/json": jsonFormat, "json": jsonFormat},
		DefaultFormat: "application/json",
		Transformers:  []huma.Transformer{completeProblem},
	}
	return huma.NewAPI(cfg, &humaAdapter{Echo: e, group: group, prefix: prefix})
}

// serveAPIDocs serves the OpenAPI document and a SwaggerUI page, both rendered once after every
// operation is registered. Huma's own routes (Config.OpenAPIPath) are not used: in v2.39.1 they
// cache the document lazily without synchronization, a data race on concurrent first requests,
// and they add YAML and OpenAPI 3.0 variants.
func serveAPIDocs(e *echo.Echo, api huma.API) {
	spec, err := json.Marshal(api.OpenAPI())
	if err != nil {
		panic("marshal OpenAPI document: " + err.Error()) // Only for unmarshalable schema examples.
	}
	e.GET("/openapi.json", func(c *echo.Context) error {
		return c.Blob(http.StatusOK, "application/openapi+json", spec)
	})
	e.GET("/docs", func(c *echo.Context) error {
		c.Response().Header().Set("Content-Security-Policy", docsCSP)
		return c.HTMLBlob(http.StatusOK, []byte(docsPage))
	})
}

// docsPage and docsCSP are Huma's SwaggerUI renderer from v2.39.1 (Copyright 2020 Daniel G. Taylor,
// MIT License: https://github.com/danielgtaylor/huma/blob/v2.39.1/LICENSE.md). The page loads
// pinned, integrity-checked assets from unpkg.com, so browsers need access to it; the hash in
// docsCSP allows exactly the inline script below.
const (
	docsCSP = "default-src 'none'; base-uri 'none'; connect-src 'self'; form-action 'none'; " +
		"frame-ancestors 'none'; sandbox allow-same-origin allow-scripts allow-popups " +
		"allow-popups-to-escape-sandbox allow-forms allow-downloads; " +
		"script-src https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js 'sha256-gRya58TMnKTH/Tne/zBInjBwFUxL66aMDYvPuAX0lNY='; " +
		"style-src https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css"
	docsPage = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>userapi Reference</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css" crossorigin integrity="sha384-KX9Rx9vM1AmUNAn07bPAiZhFD4C8jdNgG6f5MRNvR+EfAxs2PmMFtUUazui7ryZQ">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js" crossorigin integrity="sha384-o9idN8HE6/V6SAewgnr6/5nz7+Npt5J0Cb4tNyXK8pycsVmgl1ZNbRS7tlEGxd+J"></script>
    <script data-url="/openapi.json">
      const script = document.currentScript;
      const url = script.dataset.url;
      const config = script.dataset.config ? JSON.parse(script.dataset.config) : {};
      window.onload = () => {
        window.ui = SwaggerUIBundle({
          ...config,
          url: url,
          dom_id: '#swagger-ui',
        });
      };
    </script>
  </body>
</html>`
)

// removeUnusedSchemas drops the schemas Huma registers for its own error model, which this API
// never returns (errors are Problem documents).
func removeUnusedSchemas(api huma.API) {
	schemas := api.OpenAPI().Components.Schemas.Map()
	delete(schemas, "ErrorModel")
	delete(schemas, "ErrorDetail")
}

// jsonFormat encodes like Echo (HTML-escaped, newline-terminated) and problems like the
// HTTP error handler (unterminated), so responses keep the bytes they had before Huma.
var jsonFormat = huma.Format{
	Marshal: func(w io.Writer, v any) error {
		if p, ok := v.(problem); ok {
			body, err := json.Marshal(p)
			if err != nil {
				return err
			}
			_, err = w.Write(body)
			return err
		}
		return json.NewEncoder(w).Encode(v)
	},
	Unmarshal: json.Unmarshal,
}

// apiError is a handler error on its way to the client: toProblem decides the response, and
// the cause stays available for the access log.
type apiError struct {
	problem
	cause error
}

func (e *apiError) Error() string             { return e.cause.Error() }
func (e *apiError) Unwrap() error             { return e.cause }
func (e *apiError) GetStatus() int            { return e.Status }
func (e *apiError) ContentType(string) string { return problemContentType }

// completeProblem fills the request-specific parts of an error response and reports its cause
// to Echo. Huma applies it to every response body.
func completeProblem(ctx huma.Context, _ string, v any) (any, error) {
	e, ok := v.(*apiError)
	if !ok {
		return v, nil
	}
	reportError(ctx, e.cause)
	p := e.problem
	p.Type, p.Title, p.Instance = "about:blank", statusText(p.Status), ctx.URL().Path
	return p, nil
}

// register adds an operation whose handler errors are mapped by toProblem. op.RequestBody is
// only documented: given a request body schema, Huma would read and validate the body itself.
func register[I, O any](api huma.API, op huma.Operation, handler func(context.Context, *I) (*O, error)) {
	body := op.RequestBody
	op.RequestBody = nil
	huma.Register(api, op, func(ctx context.Context, in *I) (*O, error) {
		out, err := handler(ctx, in)
		if err != nil {
			return nil, &apiError{problem: toProblem(err), cause: err}
		}
		return out, nil
	})
	if body == nil {
		return
	}
	item := api.OpenAPI().Paths[op.Path]
	for _, documented := range []*huma.Operation{item.Post, item.Put, item.Patch} {
		if documented != nil && documented.OperationID == op.OperationID {
			documented.RequestBody = body
			return
		}
	}
	panic("request body of " + op.OperationID + " is not documented")
}

// jsonBody documents an optional JSON request body (see register).
func jsonBody[T any](api huma.API) *huma.RequestBody {
	schema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
	return &huma.RequestBody{Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
}

// problemResponses documents error responses as application/problem+json Problem documents.
func problemResponses(api huma.API, statuses ...int) map[string]*huma.Response {
	schema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[problem](), true, "")
	responses := make(map[string]*huma.Response, len(statuses))
	for _, status := range statuses {
		responses[strconv.Itoa(status)] = &huma.Response{
			Description: http.StatusText(status),
			Content:     map[string]*huma.MediaType{problemContentType: {Schema: schema}},
		}
	}
	return responses
}
