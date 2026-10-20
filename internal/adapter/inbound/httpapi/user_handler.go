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

// ---------- DTO: JSON wire format, separate from domain ----------

type createUserRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

// Pointers distinguish an omitted field (nil) from an explicitly empty string.
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

// etag is a strong ETag based on version: the same resource version has the same JSON representation.
func etag(u domain.User) string { return `"` + strconv.FormatInt(u.Version, 10) + `"` }

// parseIfMatch returns the version expected by the client; 0 for an absent header or "*".
// A header other than a strong ETag of the form "N" cannot match => 412 per RFC 9110.
// (Weak ETags W/"N" never match because If-Match uses strong comparison.)
// Empty headers and lists of ETags also return 412: reject instead of overwriting unconditionally.
func parseIfMatch(c *echo.Context) (int64, error) {
	values := c.Request().Header.Values("If-Match")
	if len(values) == 0 {
		return 0, nil
	}
	raw := strings.TrimSpace(strings.Join(values, ","))
	if raw == "*" {
		return 0, nil
	}
	// Strong comparison matches the issued ETag ("<version>") character for character, accepting only:
	// no sign, leading zeros, or whitespace ("+2" and "02" do not match "2").
	if len(raw) >= 3 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		s := raw[1 : len(raw)-1]
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 && strconv.FormatInt(v, 10) == s {
			return v, nil
		}
	}
	return 0, errPreconditionFailed
}

var errPreconditionFailed = echo.NewHTTPError(http.StatusPreconditionFailed, "If-Match does not match the current version of the resource")

// bindJSON reads JSON into target. Unlike echo.BindBody, it accepts only application/json (BindBody
// also accepts forms and XML, silently ignoring incorrectly formatted bodies), compares media types
// case-insensitively per RFC 9110 (unlike BindBody), and explains malformed bodies in detail.
// An absent body (Content-Length 0) is a no-op.
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
	// BodyLimit reports excess size as a body read error, which Deserialize wraps as 400.
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
		return err // *echo.HTTPError 400/413/415; the error handler handles the response.
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
		// With If-Match, a version conflict means the precondition failed (412).
		// Without If-Match, conflicts remaining after service retries become 409 (see toProblem).
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
