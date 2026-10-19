package httpapi

import (
	"net/http"
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
		Email:     u.Email,
		Name:      u.Name,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
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
	return c.JSON(http.StatusCreated, toResponse(u))
}

func (h *userHandler) get(c *echo.Context) error {
	u, err := h.users.Get(c.Request().Context(), domain.UserID(c.Param("id")))
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toResponse(u))
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
	var req updateUserRequest
	if err := echo.BindBody(c, &req); err != nil {
		return err
	}
	u, err := h.users.Update(c.Request().Context(), port.UpdateUserCmd{
		ID:      domain.UserID(c.Param("id")),
		Changes: domain.Changes{Email: req.Email, Name: req.Name},
	})
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, toResponse(u))
}

func (h *userHandler) delete(c *echo.Context) error {
	if err := h.users.Delete(c.Request().Context(), domain.UserID(c.Param("id"))); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}
