package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// ---------- DTO: hình dạng JSON trên dây, tách khỏi domain ----------

type createUserRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Con trỏ để phân biệt "không gửi field" (nil) với "gửi chuỗi rỗng".
type updateUserRequest struct {
	Email *string `json:"email"`
	Name  *string `json:"name"`
}

type userResponse struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Version   int64     `json:"version"`
}

type pageMeta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

type listUsersResponse struct {
	Data []userResponse `json:"data"`
	Meta pageMeta       `json:"meta"`
}

func toResponse(u domain.User) userResponse {
	return userResponse{
		ID:        u.ID.String(),
		Email:     u.Email.String(),
		Name:      u.Name,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
		Version:   u.Version,
	}
}

// etag là strong ETag dựa trên version: cùng version thì cùng biểu diễn JSON.
func etag(u domain.User) string { return `"` + strconv.FormatInt(u.Version, 10) + `"` }

// parseIfMatch trả version client mong đợi; 0 nếu không có header hoặc là "*".
// Header không phải một strong ETag dạng "N" thì không thể khớp => 412 theo RFC 9110.
// (Weak ETag W/"N" cũng không bao giờ khớp vì If-Match dùng so sánh strong.)
func parseIfMatch(c *echo.Context) (int64, error) {
	raw := strings.TrimSpace(c.Request().Header.Get("If-Match"))
	if raw == "" || raw == "*" {
		return 0, nil
	}
	if len(raw) >= 3 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		if v, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64); err == nil && v > 0 {
			return v, nil
		}
	}
	return 0, errPreconditionFailed
}

var errPreconditionFailed = echo.NewHTTPError(http.StatusPreconditionFailed, "If-Match does not match the current version of the resource")

func writeUser(c *echo.Context, status int, u domain.User) error {
	c.Response().Header().Set("ETag", etag(u))
	return c.JSON(status, toResponse(u))
}

// ---------- Handler ----------

type userHandler struct {
	users port.UserService
}

func (h *userHandler) create(c *echo.Context) error {
	var req createUserRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err // *echo.HTTPError 400/415, error handler lo phần còn lại
	}
	u, err := h.users.Register(c.Request().Context(), port.RegisterUserCmd{Email: req.Email, Name: req.Name})
	if err != nil {
		return err
	}
	c.Response().Header().Set(echo.HeaderLocation, "/api/v1/users/"+u.ID.String())
	return writeUser(c, http.StatusCreated, u)
}

func (h *userHandler) get(c *echo.Context) error {
	u, err := h.users.Get(c.Request().Context(), domain.UserID(c.Param("id")))
	if err != nil {
		return err
	}
	return writeUser(c, http.StatusOK, u)
}

func (h *userHandler) list(c *echo.Context) error {
	limit, err := echo.QueryParamOr[int](c, "limit", 0)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "limit must be an integer")
	}
	offset, err := echo.QueryParamOr[int](c, "offset", 0)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "offset must be an integer")
	}
	page, err := h.users.List(c.Request().Context(), port.ListUsersQuery{Limit: limit, Offset: offset})
	if err != nil {
		return err
	}
	data := make([]userResponse, len(page.Users))
	for i, u := range page.Users {
		data[i] = toResponse(u)
	}
	return c.JSON(http.StatusOK, listUsersResponse{
		Data: data,
		Meta: pageMeta{Limit: page.Limit, Offset: page.Offset, Total: page.Total},
	})
}

func (h *userHandler) update(c *echo.Context) error {
	expected, err := parseIfMatch(c)
	if err != nil {
		return err
	}
	var req updateUserRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	u, err := h.users.Update(c.Request().Context(), port.UpdateUserCmd{
		ID:              domain.UserID(c.Param("id")),
		Changes:         domain.Changes{Email: req.Email, Name: req.Name},
		ExpectedVersion: expected,
	})
	if err != nil {
		// Client đã gửi If-Match: xung đột version là điều kiện tiên quyết không thoả (412).
		// Không có If-Match: service đã tự retry mà vẫn xung đột => 409 (xem toProblem).
		if expected != 0 && errors.Is(err, domain.ErrVersionConflict) {
			return errPreconditionFailed
		}
		return err
	}
	return writeUser(c, http.StatusOK, u)
}

func (h *userHandler) delete(c *echo.Context) error {
	if err := h.users.Delete(c.Request().Context(), domain.UserID(c.Param("id"))); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
