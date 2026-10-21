// Package httpapi is the inbound HTTP adapter: Echo v5 routes and middleware, Huma v2 typed operations and OpenAPI.
// It depends on the inbound port (port.UserService), without knowing storage details.
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/observability"
)

type Options struct {
	Logger *slog.Logger
	// Ready returns an error when the pod should not receive traffic (shutting down, starting...).
	// nil = always ready. Check only this pod's own state; see docs/operations.md.
	Ready func(ctx context.Context) error
	// RequestTimeout applies to each request context. 0 = default 10s.
	RequestTimeout time.Duration
	// TrustedProxies lists trusted reverse proxy / load balancer CIDRs. When set, the client IP
	// comes from X-Forwarded-For (trusting only hops in these ranges). Empty = TCP connection IP.
	TrustedProxies []*net.IPNet
	// APIMiddleware applies to /api/v1 (not probes), for example authentication or rate limiting.
	// This template has NO authentication: attach auth middleware here before production deployment.
	APIMiddleware []echo.MiddlewareFunc
	// APIDocs serves the generated OpenAPI 3.1 document at /openapi.json and SwaggerUI at /docs.
	// These routes are public: APIMiddleware does not apply to them.
	APIDocs bool
}

// probePaths bypass the request logger: probes run every few seconds on each pod,
// so logging them adds ingestion costs without useful information.
var probePaths = map[string]bool{"/healthz": true, "/readyz": true}

const bodyLimit = 1 << 20 // 1 MiB

// NewServer builds an *echo.Echo with middleware, error handling, and routes.
func NewServer(users port.UserService, opts Options) *echo.Echo {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 10 * time.Second
	}

	e := echo.New()
	e.Logger = opts.Logger
	e.IPExtractor = ipExtractor(opts.TrustedProxies)
	e.HTTPErrorHandler = newErrorHandler(opts.Logger)

	// Order matters: RequestID first supplies request_id to the logger; traceRoute names spans by
	// route; Recover follows the logger so panics become logged 500 responses.
	e.Pre(dropInvalidRequestID)
	e.Use(middleware.RequestIDWithConfig(middleware.RequestIDConfig{
		// Attach request_id to ctx so every slog.*Context(ctx) log at any layer includes it.
		RequestIDHandler: func(c *echo.Context, id string) {
			ctx := observability.WithLogAttrs(c.Request().Context(), slog.String("request_id", id))
			c.SetRequest(c.Request().WithContext(ctx))
		},
	}))
	e.Use(traceRoute)
	e.Use(requestLogger(opts.Logger))
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(bodyLimit))
	e.Use(middleware.ContextTimeout(opts.RequestTimeout))

	e.GET("/healthz", func(c *echo.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})
	e.GET("/readyz", func(c *echo.Context) error {
		if opts.Ready != nil {
			if err := opts.Ready(c.Request().Context()); err != nil {
				opts.Logger.Warn("readiness check failed", "error", err)
				return echo.ErrServiceUnavailable
			}
		}
		return c.JSON(http.StatusOK, map[string]string{"status": "ready"})
	})

	h := &userHandler{users: users}
	v1 := e.Group("/api/v1", opts.APIMiddleware...)
	api := newHumaAPI(e, v1, "/api/v1")
	h.register(api)
	removeUnusedSchemas(api)
	if opts.APIDocs {
		serveAPIDocs(e, api)
	}

	return e
}

// dropInvalidRequestID removes invalid client-supplied X-Request-Id values so RequestID
// middleware generates a new ID. IDs from ingress or callers link logs across services, but
// they enter EVERY log entry and must be short and contain only safe characters.
func dropInvalidRequestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if id := c.Request().Header.Get(echo.HeaderXRequestID); id != "" && !validRequestID(id) {
			c.Request().Header.Del(echo.HeaderXRequestID)
		}
		return next(c)
	}
}

var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`).MatchString

// Instrument wraps Echo with OpenTelemetry at the net/http layer, outside Echo, so spans and
// metrics record the actual status of EVERY response, including errors written by HTTPErrorHandler.
// Probes are not traced. Without OpenTelemetry configuration, global providers are no-ops.
func Instrument(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !probePaths[r.URL.Path] }),
	)
}

// traceRoute names spans "METHOD /route/:param" and sets http.route on both spans and metrics.
// Use the registered route, not the actual URL, to keep cardinality low.
func traceRoute(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		route := c.Path()
		if route == "" {
			return next(c)
		}
		ctx := c.Request().Context()
		span := trace.SpanFromContext(ctx)
		span.SetName(c.Request().Method + " " + route)
		span.SetAttributes(attribute.String("http.route", route))
		if l, ok := otelhttp.LabelerFromContext(ctx); ok {
			l.Add(attribute.String("http.route", route))
		}
		return next(c)
	}
}

func ipExtractor(trusted []*net.IPNet) echo.IPExtractor {
	if len(trusted) == 0 {
		return echo.ExtractIPDirect()
	}
	// Disable defaults (loopback, link-local, private networks) to trust ONLY configured ranges:
	// other cluster pods also use private addresses and could forge X-Forwarded-For.
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, n := range trusted {
		opts = append(opts, echo.TrustIPRange(n))
	}
	return echo.ExtractIPFromXFFHeader(opts...)
}

// requestLogger logs each non-probe request: 2xx/3xx = INFO, 4xx = WARN, 5xx = ERROR.
// (Echo's default logger logs every request error at ERROR, including 404/422.)
func requestLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper: func(c *echo.Context) bool {
			return probePaths[c.Request().URL.Path]
		},
		LogStatus:    true,
		LogMethod:    true,
		LogURIPath:   true, // Path only: query strings can carry tokens or PII.
		LogRoutePath: true,
		LogLatency:   true,
		LogRemoteIP:  true,
		// Pass the error to HTTPErrorHandler before logging to obtain the actual status.
		HandleError: true,
		LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			switch {
			case v.Status >= 500:
				level = slog.LevelError
			case v.Status >= 400:
				level = slog.LevelWarn
			}
			attrs := []slog.Attr{
				slog.String("method", v.Method),
				slog.String("path", v.URIPath),
				slog.String("route", v.RoutePath),
				slog.Int("status", v.Status),
				slog.Duration("latency", v.Latency),
				slog.String("remote_ip", v.RemoteIP),
				// observability.ContextHandler adds request_id and trace_id from ctx.
			}
			if v.Error != nil && v.Status >= 500 {
				attrs = append(attrs, slog.String("error", v.Error.Error()))
			}
			logger.LogAttrs(c.Request().Context(), level, "http_request", attrs...)
			return nil
		},
	})
}
