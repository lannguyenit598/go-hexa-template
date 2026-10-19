package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/core/domain"
)

// problem là body lỗi theo RFC 9457 (application/problem+json),
// mở rộng thêm "errors" cho lỗi validation theo từng trường.
type problem struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Errors   []fieldError `json:"errors,omitempty"`
}

// fieldError là DTO của domain.FieldError: định dạng JSON thuộc về adapter, không thuộc domain.
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func toFieldErrors(in []domain.FieldError) []fieldError {
	out := make([]fieldError, len(in))
	for i, f := range in {
		out[i] = fieldError{Field: f.Field, Message: f.Message}
	}
	return out
}

const problemContentType = "application/problem+json"

// StatusClientClosedRequest (quy ước của nginx): client đã đóng kết nối trước khi có
// response. Không phải lỗi của server nên không được tính là 5xx, tránh làm sai
// error rate trên SLO dashboard và bắn alert giả mỗi khi mobile client đổi mạng.
const StatusClientClosedRequest = 499

func statusText(code int) string {
	if code == StatusClientClosedRequest {
		return "Client Closed Request"
	}
	return http.StatusText(code)
}

// toProblem là nơi DUY NHẤT ánh xạ lỗi domain sang HTTP.
func toProblem(err error) problem {
	var ve *domain.ValidationError
	switch {
	case errors.As(err, &ve):
		return problem{Status: http.StatusUnprocessableEntity, Detail: "request failed validation", Errors: toFieldErrors(ve.Fields)}
	case errors.Is(err, domain.ErrUserNotFound):
		return problem{Status: http.StatusNotFound, Detail: "user not found"}
	case errors.Is(err, domain.ErrEmailAlreadyExists):
		return problem{Status: http.StatusConflict, Detail: "a user with this email already exists"}
	case errors.Is(err, domain.ErrVersionConflict):
		return problem{Status: http.StatusConflict, Detail: "the user was modified concurrently, please retry"}
	case errors.Is(err, context.DeadlineExceeded):
		return problem{Status: http.StatusServiceUnavailable, Detail: "request timed out"}
	case errors.Is(err, context.Canceled):
		return problem{Status: StatusClientClosedRequest, Detail: "client closed request"}
	}

	// Lỗi do Echo sinh ra (404 route, 405, bind 400, 413 body quá lớn...).
	if code := echo.StatusCode(err); code != 0 {
		p := problem{Status: code}
		var he *echo.HTTPError
		if errors.As(err, &he) && code < 500 {
			p.Detail = he.Message
		}
		return p
	}

	// Lỗi không lường trước: không lộ chi tiết nội bộ ra ngoài.
	return problem{Status: http.StatusInternalServerError}
}

// newErrorHandler ghi lỗi ra client. Việc log do requestLogger đảm nhận (một dòng mỗi request).
func newErrorHandler(logger *slog.Logger) echo.HTTPErrorHandler {
	return func(c *echo.Context, err error) {
		if resp, _ := echo.UnwrapResponse(c.Response()); resp != nil && resp.Committed {
			return
		}

		p := toProblem(err)
		p.Type = "about:blank"
		p.Title = statusText(p.Status)
		p.Instance = c.Request().URL.Path

		if c.Request().Method == http.MethodHead {
			_ = c.NoContent(p.Status)
			return
		}
		body, mErr := json.Marshal(p)
		if mErr != nil {
			_ = c.NoContent(http.StatusInternalServerError)
			return
		}
		if wErr := c.Blob(p.Status, problemContentType, body); wErr != nil {
			logger.Error("failed to write error response", "error", wErr)
		}
	}
}
