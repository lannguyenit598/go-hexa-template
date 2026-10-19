// Package httpapi là adapter inbound HTTP dùng Echo v5.
// Nó chỉ phụ thuộc vào inbound port (port.UserService), không biết storage là gì.
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
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
	// Ready trả lỗi khi pod không nên nhận traffic (đang shutdown, chưa khởi động xong...).
	// nil = luôn sẵn sàng. Chỉ nên kiểm tra trạng thái CỦA CHÍNH pod; xem docs/operations.md.
	Ready func(ctx context.Context) error
	// RequestTimeout áp cho context của mỗi request. 0 = mặc định 10s.
	RequestTimeout time.Duration
	// BodyLimit tính bằng byte. 0 = mặc định 1 MiB.
	BodyLimit int64
	// TrustedProxies: dải CIDR của reverse proxy / load balancer tin cậy. Khi có, IP client
	// lấy từ X-Forwarded-For (chỉ tin các hop nằm trong dải này). Rỗng = IP của kết nối TCP.
	TrustedProxies []*net.IPNet
	// APIMiddleware áp cho nhóm /api/v1 (không áp cho probe), ví dụ xác thực, rate limit.
	// Template KHÔNG có xác thực: hãy gắn middleware auth ở đây trước khi đưa lên production.
	APIMiddleware []echo.MiddlewareFunc
}

// probePaths không đi qua request logger: probe gọi vài giây một lần trên mỗi pod,
// log chúng chỉ tốn chi phí log ingestion mà không có giá trị.
var probePaths = map[string]bool{"/healthz": true, "/readyz": true}

// NewServer dựng *echo.Echo với middleware, error handler và route.
func NewServer(users port.UserService, opts Options) *echo.Echo {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 10 * time.Second
	}
	if opts.BodyLimit == 0 {
		opts.BodyLimit = 1 << 20
	}

	e := echo.New()
	e.Logger = opts.Logger
	e.IPExtractor = ipExtractor(opts.TrustedProxies)
	e.HTTPErrorHandler = newErrorHandler(opts.Logger)

	// Thứ tự quan trọng: RequestID trước để logger có request_id; traceRoute đặt tên span theo
	// route; Recover ngay sau logger để panic được chuyển thành lỗi 500 và vẫn được log.
	e.Pre(dropInvalidRequestID)
	e.Use(middleware.RequestIDWithConfig(middleware.RequestIDConfig{
		// Gắn request_id vào ctx: mọi dòng log slog.*Context(ctx) ở bất kỳ tầng nào đều mang nó.
		RequestIDHandler: func(c *echo.Context, id string) {
			ctx := observability.WithLogAttrs(c.Request().Context(), slog.String("request_id", id))
			c.SetRequest(c.Request().WithContext(ctx))
		},
	}))
	e.Use(traceRoute)
	e.Use(requestLogger(opts.Logger))
	e.Use(middleware.Recover())
	e.Use(middleware.BodyLimit(opts.BodyLimit))
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
	v1.POST("/users", h.create)
	v1.GET("/users", h.list)
	v1.GET("/users/:id", h.get)
	v1.PATCH("/users/:id", h.update)
	v1.DELETE("/users/:id", h.delete)

	return e
}

// dropInvalidRequestID bỏ X-Request-Id do client gửi nếu không hợp lệ, để middleware RequestID
// sinh ID mới. ID được tin từ ingress / service gọi tới (để nối log xuyên service), nhưng vì nó
// đi vào MỌI dòng log nên phải ngắn và chỉ gồm ký tự an toàn.
func dropInvalidRequestID(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if id := c.Request().Header.Get(echo.HeaderXRequestID); id != "" && !validRequestID(id) {
			c.Request().Header.Del(echo.HeaderXRequestID)
		}
		return next(c)
	}
}

func validRequestID(id string) bool {
	if len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// Instrument bọc Echo bằng OpenTelemetry ở tầng net/http (bên ngoài Echo), để span và metric
// ghi nhận đúng status code của MỌI response, kể cả response lỗi do HTTPErrorHandler ghi.
// Probe không được trace. Khi OpenTelemetry chưa được cấu hình, provider toàn cục là no-op.
func Instrument(h http.Handler) http.Handler {
	return otelhttp.NewHandler(h, "http.server",
		otelhttp.WithFilter(func(r *http.Request) bool { return !probePaths[r.URL.Path] }),
	)
}

// traceRoute đặt tên span là "METHOD /route/:param" và gắn http.route cho cả span lẫn metric.
// Dùng route đã đăng ký (không phải URL thật) để giữ cardinality thấp.
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
	// Tắt các mặc định (loopback, link-local, mạng private) để CHỈ tin đúng dải được cấu hình:
	// trong cluster, mọi pod khác cũng nằm trong mạng private và có thể giả X-Forwarded-For.
	opts := []echo.TrustOption{echo.TrustLoopback(false), echo.TrustLinkLocal(false), echo.TrustPrivateNet(false)}
	for _, n := range trusted {
		opts = append(opts, echo.TrustIPRange(n))
	}
	return echo.ExtractIPFromXFFHeader(opts...)
}

// requestLogger ghi một dòng log cho mỗi request (trừ probe): 2xx/3xx = INFO, 4xx = WARN, 5xx = ERROR.
// (Logger mặc định của Echo ghi mọi request có lỗi ở mức ERROR, kể cả 404/422.)
func requestLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		Skipper: func(c *echo.Context) bool {
			return probePaths[c.Request().URL.Path]
		},
		LogStatus:    true,
		LogMethod:    true,
		LogURI:       true,
		LogRoutePath: true,
		LogLatency:   true,
		LogRemoteIP:  true,
		// Chuyển lỗi cho HTTPErrorHandler trước khi log để biết status thật.
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
				slog.String("uri", v.URI),
				slog.String("route", v.RoutePath),
				slog.Int("status", v.Status),
				slog.Duration("latency", v.Latency),
				slog.String("remote_ip", v.RemoteIP),
				// request_id, trace_id do observability.ContextHandler thêm từ ctx.
			}
			if v.Error != nil && v.Status >= 500 {
				attrs = append(attrs, slog.String("error", v.Error.Error()))
			}
			logger.LogAttrs(c.Request().Context(), level, "http_request", attrs...)
			return nil
		},
	})
}
