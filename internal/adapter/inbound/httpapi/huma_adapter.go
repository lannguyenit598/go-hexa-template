package httpapi

import (
	"context"
	"crypto/tls"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v5"
)

// humaAdapter runs Huma operations on the Echo v5 router. It follows the Echo v5 adapter in
// github.com/danielgtaylor/huma/v2/adapters/humaecho v2.39.1 (Copyright 2020 Daniel G. Taylor,
// MIT License: https://github.com/danielgtaylor/huma/blob/v2.39.1/LICENSE.md), which is not
// imported because that package also contains an Echo v4 adapter and would link Echo v4 and its
// dependencies into the binary.
//
// Differences from humaecho:
//   - Operations register on group, so they inherit its middleware (APIMiddleware) exactly like
//     native routes, while the OpenAPI document keeps full paths. An operation outside prefix
//     panics instead of silently skipping that middleware.
//   - The handler error passed to reportError is returned to Echo, so the access log records
//     the cause of a 5xx response as it does for native routes.
//   - SetStatus does not write a status after the response is committed.
//   - Header hides Accept from content negotiation; see Header.
type humaAdapter struct {
	*echo.Echo
	group  *echo.Group
	prefix string
}

func (a *humaAdapter) Handle(op *huma.Operation, handler func(huma.Context)) {
	rest, ok := strings.CutPrefix(op.Path, a.prefix+"/")
	if !ok {
		panic("huma operation " + op.Method + " " + op.Path + " is outside " + a.prefix)
	}
	path := strings.NewReplacer("{", ":", "}", "").Replace("/" + rest) // {id} -> :id
	a.group.Add(op.Method, path, func(c *echo.Context) error {
		state := &humaState{}
		handler(&humaContext{op: op, orig: c, state: state})
		return state.err
	})
}

// humaState is shared by every WithContext copy of a request's humaContext.
type humaState struct {
	status int
	err    error
}

type humaContext struct {
	op    *huma.Operation
	orig  *echo.Context
	state *humaState
}

var _ huma.Context = (*humaContext)(nil)

// echoContext returns the Echo context behind a Huma context created by humaAdapter.
func echoContext(ctx huma.Context) *echo.Context {
	return unwrapHuma(ctx).orig
}

// reportError hands err to Echo once Huma has written the response; see humaAdapter.
func reportError(ctx huma.Context, err error) {
	unwrapHuma(ctx).state.err = err
}

func unwrapHuma(ctx huma.Context) *humaContext {
	for {
		w, ok := ctx.(interface{ Unwrap() huma.Context })
		if !ok {
			break
		}
		ctx = w.Unwrap()
	}
	return ctx.(*humaContext) // Every operation runs through humaAdapter.
}

func (c *humaContext) Operation() *huma.Operation   { return c.op }
func (c *humaContext) Context() context.Context     { return c.orig.Request().Context() }
func (c *humaContext) TLS() *tls.ConnectionState    { return c.orig.Request().TLS }
func (c *humaContext) Method() string               { return c.orig.Request().Method }
func (c *humaContext) Host() string                 { return c.orig.Request().Host }
func (c *humaContext) RemoteAddr() string           { return c.orig.Request().RemoteAddr }
func (c *humaContext) URL() url.URL                 { return *c.orig.Request().URL }
func (c *humaContext) Param(name string) string     { return c.orig.Param(name) }
func (c *humaContext) Query(name string) string     { return c.orig.QueryParam(name) }
func (c *humaContext) BodyReader() io.Reader        { return c.orig.Request().Body }
func (c *humaContext) BodyWriter() io.Writer        { return c.orig.Response() }
func (c *humaContext) Status() int                  { return c.state.status }
func (c *humaContext) SetHeader(name, value string) { c.orig.Response().Header().Set(name, value) }

// Header returns "" for Accept so Huma always picks the default format: the API has ignored
// Accept since before Huma, and negotiation would echo tokens such as "json" (a format key, not
// a media type) as the Content-Type.
func (c *humaContext) Header(name string) string {
	if http.CanonicalHeaderKey(name) == "Accept" {
		return ""
	}
	return c.orig.Request().Header.Get(name)
}

func (c *humaContext) AppendHeader(name, value string) {
	c.orig.Response().Header().Add(name, value)
}

func (c *humaContext) EachHeader(cb func(name, value string)) {
	for name, values := range c.orig.Request().Header {
		for _, value := range values {
			cb(name, value)
		}
	}
}

// GetMultipartForm is unused: no operation accepts multipart bodies.
func (c *humaContext) GetMultipartForm() (*multipart.Form, error) {
	err := c.orig.Request().ParseMultipartForm(8 << 10)
	return c.orig.Request().MultipartForm, err
}

func (c *humaContext) SetReadDeadline(deadline time.Time) error {
	return huma.SetReadDeadline(c.orig.Response(), deadline)
}

// SetStatus records code and writes it unless the response is committed: Huma sets 499 after a
// client disconnects mid-body, and Echo logs a second WriteHeader at ERROR.
func (c *humaContext) SetStatus(code int) {
	c.state.status = code
	if resp, _ := echo.UnwrapResponse(c.orig.Response()); resp != nil && resp.Committed {
		return
	}
	c.orig.Response().WriteHeader(code)
}

func (c *humaContext) Version() huma.ProtoVersion {
	r := c.orig.Request()
	return huma.ProtoVersion{Proto: r.Proto, ProtoMajor: r.ProtoMajor, ProtoMinor: r.ProtoMinor}
}

// WithContext replaces the request context in place (as humaecho does) so Echo middleware sees
// values added by Huma middleware.
func (c *humaContext) WithContext(ctx context.Context) huma.Context {
	c.orig.SetRequest(c.orig.Request().WithContext(ctx))
	return &humaContext{op: c.op, orig: c.orig, state: c.state}
}

var _ huma.Adapter = (*humaAdapter)(nil)
