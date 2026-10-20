package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
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
// Header rỗng hoặc danh sách nhiều ETag cũng trả 412: thà từ chối còn hơn ghi đè vô điều kiện.
func parseIfMatch(c *echo.Context) (int64, error) {
	values := c.Request().Header.Values("If-Match")
	if len(values) == 0 {
		return 0, nil
	}
	raw := strings.TrimSpace(strings.Join(values, ","))
	if raw == "*" {
		return 0, nil
	}
	// So khớp strong là so từng ký tự với ETag đã cấp ("<version>"), nên chỉ nhận đúng dạng đó:
	// không dấu, không số 0 ở đầu, không khoảng trắng ("+2", "02" không khớp "2").
	if len(raw) >= 3 && raw[0] == '"' && raw[len(raw)-1] == '"' && isCanonicalVersion(raw[1:len(raw)-1]) {
		if v, err := strconv.ParseInt(raw[1:len(raw)-1], 10, 64); err == nil {
			return v, nil
		}
	}
	return 0, errPreconditionFailed
}

// isCanonicalVersion: chuỗi là số nguyên dương viết như strconv.FormatInt viết ra.
func isCanonicalVersion(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

var errPreconditionFailed = echo.NewHTTPError(http.StatusPreconditionFailed, "If-Match does not match the current version of the resource")

// bindJSON đọc body JSON vào target. Khác echo.BindBody, nó chỉ nhận application/json (BindBody
// còn nhận form và XML, khiến body sai định dạng bị bỏ qua âm thầm), so media type không phân biệt
// hoa thường như RFC 9110 (BindBody thì có), và trả detail cho biết vì sao body hỏng. Không có
// body (Content-Length 0) thì không làm gì.
func bindJSON(c *echo.Context, target any) error {
	req := c.Request()
	if req.ContentLength == 0 {
		return nil
	}
	mediaType, _, _ := strings.Cut(req.Header.Get(echo.HeaderContentType), ";")
	if !strings.EqualFold(strings.TrimSpace(mediaType), echo.MIMEApplicationJSON) {
		return echo.ErrUnsupportedMediaType
	}
	err := c.Echo().JSONSerializer.Deserialize(c, target)
	if err == nil {
		return nil
	}
	// BodyLimit báo vượt giới hạn qua lỗi đọc body, bị Deserialize bọc thành 400.
	if errors.Is(err, echo.ErrStatusRequestEntityTooLarge) {
		return echo.ErrStatusRequestEntityTooLarge
	}
	var (
		syntaxErr *json.SyntaxError
		typeErr   *json.UnmarshalTypeError
	)
	switch {
	case errors.As(err, &syntaxErr):
		return echo.NewHTTPError(http.StatusBadRequest, "request body is not valid JSON").Wrap(err)
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("field %q has the wrong type", typeErr.Field)).Wrap(err)
	case errors.As(err, &typeErr):
		return echo.NewHTTPError(http.StatusBadRequest, "request body must be a JSON object").Wrap(err)
	}
	return err
}

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
	if err := bindJSON(c, &req); err != nil {
		return err // *echo.HTTPError 400/413/415, error handler lo phần còn lại
	}
	u, err := h.users.Register(c.Request().Context(), port.RegisterUserCmd{Email: req.Email, Name: req.Name})
	if err != nil {
		return err
	}
	c.Response().Header().Set(echo.HeaderLocation, "/api/v1/users/"+u.ID.String())
	return writeUser(c, http.StatusCreated, u)
}

func (h *userHandler) get(c *echo.Context) error {
	u, err := h.users.Get(c.Request().Context(), c.Param("id"))
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
	if err := bindJSON(c, &req); err != nil {
		return err
	}
	u, err := h.users.Update(c.Request().Context(), port.UpdateUserCmd{
		ID:              c.Param("id"),
		Email:           req.Email,
		Name:            req.Name,
		ExpectedVersion: expected,
	})
	if err != nil {
		// Client đã gửi If-Match: xung đột version là điều kiện tiên quyết không thoả (412).
		// Không có If-Match: service đã tự retry mà vẫn xung đột => 409 (xem toProblem).
		if expected != 0 && errors.Is(err, port.ErrVersionConflict) {
			return errPreconditionFailed
		}
		return err
	}
	return writeUser(c, http.StatusOK, u)
}

func (h *userHandler) delete(c *echo.Context) error {
	if err := h.users.Delete(c.Request().Context(), c.Param("id")); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
