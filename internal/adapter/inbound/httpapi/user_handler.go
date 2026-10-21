package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/labstack/echo/v5"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// ---------- DTO: JSON wire format, separate from domain ----------

// The doc tags describe the OpenAPI schema; normalization and validation happen in the domain,
// so the schemas add no length or format limits.

type createUserRequest struct {
	_     struct{} `additionalProperties:"true"` // Unknown fields are ignored.
	Email string   `json:"email" example:" Lan@Example.COM " doc:"Trimmed and lowercased; at most 254 characters; a bare address; unique."`
	Name  string   `json:"name" example:"  Lan   Nguyen " doc:"Whitespace runs collapse to one space; at most 100 Unicode code points."`
}

// Pointers distinguish an omitted field (nil) from an explicitly empty string.
type updateUserRequest struct {
	_     struct{} `additionalProperties:"true"`
	Email *string  `json:"email" required:"false" nullable:"true" doc:"Omitted or null: unchanged. Same rules as on creation."`
	Name  *string  `json:"name" required:"false" nullable:"true" example:"Lan Pham" doc:"Omitted or null: unchanged. Same rules as on creation."`
}

type userResponse struct {
	ID    string `json:"id" example:"01a15359-aa28-7a00-a380-c47e52905707"`
	Email string `json:"email" example:"lan@example.com"`
	Name  string `json:"name" example:"Lan Nguyen"`
	// Times are UTC with up to microsecond precision; trailing fractional zeros are omitted.
	CreatedAt time.Time `json:"created_at" example:"2026-10-19T08:49:07.624656Z" doc:"UTC, 0 to 6 fractional digits"`
	UpdatedAt time.Time `json:"updated_at" example:"2026-10-19T08:49:07.624656Z" doc:"Changes only when data changes"`
	Version   int64     `json:"version" example:"1" doc:"Starts at 1; increments on each actual change"`
}

type pageMeta struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
	Total  int `json:"total"`
}

type listUsersResponse struct {
	Data []userResponse `json:"data" nullable:"false"`
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

// ---------- Handler ----------

type userHandler struct {
	users port.UserService
}

// register adds the user operations. Inputs read the request in Resolve with the same helpers as
// before Huma (bindJSON, parseIfMatch, echo.QueryParamOr), keeping their status codes and
// details; Huma's own parsing and validation would answer with different errors.
func (h *userHandler) register(api huma.API) {
	tags := []string{"Users"}
	register(api, huma.Operation{
		OperationID:   "createUser",
		Method:        http.MethodPost,
		Path:          "/api/v1/users",
		Summary:       "Create a user",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		RequestBody:   jsonBody[createUserRequest](api),
		Responses:     problemResponses(api, 400, 409, 413, 415, 422, 500, 503),
	}, h.create)
	register(api, huma.Operation{
		OperationID: "listUsers",
		Method:      http.MethodGet,
		Path:        "/api/v1/users",
		Summary:     "List users",
		Description: "Ordered by created_at, then id. meta reports the effective limit and offset.",
		Tags:        tags,
		Parameters: []*huma.Param{
			{Name: "limit", In: "query", Description: "Page size. Values ≤ 0 use 20; values above 100 use 100.",
				Schema: &huma.Schema{Type: huma.TypeInteger, Default: 20}},
			{Name: "offset", In: "query", Description: "Users to skip. Negative values use 0.",
				Schema: &huma.Schema{Type: huma.TypeInteger, Default: 0, Maximum: new(10000.0)}},
		},
		Responses: problemResponses(api, 400, 422, 500, 503),
	}, h.list)
	register(api, huma.Operation{
		OperationID: "getUser",
		Method:      http.MethodGet,
		Path:        "/api/v1/users/{id}",
		Summary:     "Get a user",
		Tags:        tags,
		Responses:   problemResponses(api, 404, 500, 503),
	}, h.get)
	register(api, huma.Operation{
		OperationID: "updateUser",
		Method:      http.MethodPatch,
		Path:        "/api/v1/users/{id}",
		Summary:     "Update a user",
		Description: "Only supplied fields change; omitted or null fields stay unchanged. " +
			"An empty body or {} returns the current user without a new version.",
		Tags: tags,
		Parameters: []*huma.Param{
			{Name: "If-Match", In: "header", Schema: &huma.Schema{Type: huma.TypeString},
				Description: `Update only if the current version matches this strong ETag, such as "1". ` +
					`"*" or no header: no precondition. Other values, including weak, empty, or multiple ETags, return 412.`},
		},
		RequestBody: jsonBody[updateUserRequest](api),
		Responses:   problemResponses(api, 400, 404, 409, 412, 413, 415, 422, 500, 503),
	}, h.update)
	register(api, huma.Operation{
		OperationID:   "deleteUser",
		Method:        http.MethodDelete,
		Path:          "/api/v1/users/{id}",
		Summary:       "Delete a user",
		Description:   "If-Match is ignored.",
		Tags:          tags,
		DefaultStatus: http.StatusNoContent,
		Responses:     problemResponses(api, 404, 500, 503),
	}, h.delete)
}

// userID is the path parameter. Malformed IDs reach the service, which reports them as absent (404).
type userID struct {
	ID string `path:"id" doc:"User ID: a lowercase UUID v7. Malformed IDs return 404."`
}

type createUserInput struct {
	body createUserRequest
	err  error
}

func (in *createUserInput) Resolve(ctx huma.Context) []error {
	in.err = bindJSON(echoContext(ctx), &in.body)
	return nil
}

type listUsersInput struct {
	limit, offset int
	err           error
}

func (in *listUsersInput) Resolve(ctx huma.Context) []error {
	c := echoContext(ctx)
	var err error
	if in.limit, err = echo.QueryParamOr[int](c, "limit", 0); err != nil {
		in.err = echo.NewHTTPError(http.StatusBadRequest, "limit must be an integer")
	} else if in.offset, err = echo.QueryParamOr[int](c, "offset", 0); err != nil {
		in.err = echo.NewHTTPError(http.StatusBadRequest, "offset must be an integer")
	}
	return nil
}

type updateUserInput struct {
	ID       string `path:"id" doc:"User ID: a lowercase UUID v7. Malformed IDs return 404."`
	expected int64
	body     updateUserRequest
	err      error
}

func (in *updateUserInput) Resolve(ctx huma.Context) []error {
	c := echoContext(ctx)
	if in.expected, in.err = parseIfMatch(c); in.err == nil {
		in.err = bindJSON(c, &in.body)
	}
	return nil
}

type userOutput struct {
	ETag string `header:"ETag" doc:"Current version as a strong ETag, such as \"1\""`
	Body userResponse
}

type createUserOutput struct {
	Location string `header:"Location" doc:"Path of the new user"`
	ETag     string `header:"ETag" doc:"Current version as a strong ETag, such as \"1\""`
	Body     userResponse
}

type listUsersOutput struct {
	Body listUsersResponse
}

func newUserOutput(u domain.User) *userOutput { return &userOutput{ETag: etag(u), Body: toResponse(u)} }

// Handlers return input errors (*echo.HTTPError 400/412/413/415) and service errors; register
// maps both through toProblem.

func (h *userHandler) create(ctx context.Context, in *createUserInput) (*createUserOutput, error) {
	if in.err != nil {
		return nil, in.err
	}
	u, err := h.users.Register(ctx, port.RegisterUserCmd{Email: in.body.Email, Name: in.body.Name})
	if err != nil {
		return nil, err
	}
	return &createUserOutput{Location: "/api/v1/users/" + u.ID.String(), ETag: etag(u), Body: toResponse(u)}, nil
}

func (h *userHandler) get(ctx context.Context, in *userID) (*userOutput, error) {
	u, err := h.users.Get(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	return newUserOutput(u), nil
}

func (h *userHandler) list(ctx context.Context, in *listUsersInput) (*listUsersOutput, error) {
	if in.err != nil {
		return nil, in.err
	}
	page, err := h.users.List(ctx, port.ListUsersQuery{Limit: in.limit, Offset: in.offset})
	if err != nil {
		return nil, err
	}
	data := make([]userResponse, len(page.Users))
	for i, u := range page.Users {
		data[i] = toResponse(u)
	}
	return &listUsersOutput{Body: listUsersResponse{
		Data: data,
		Meta: pageMeta{Limit: page.Limit, Offset: page.Offset, Total: page.Total},
	}}, nil
}

func (h *userHandler) update(ctx context.Context, in *updateUserInput) (*userOutput, error) {
	if in.err != nil {
		return nil, in.err
	}
	u, err := h.users.Update(ctx, port.UpdateUserCmd{
		ID:              in.ID,
		Email:           in.body.Email,
		Name:            in.body.Name,
		ExpectedVersion: in.expected,
	})
	if err != nil {
		// With If-Match, a version conflict means the precondition failed (412).
		// Without If-Match, conflicts remaining after service retries become 409 (see toProblem).
		if in.expected != 0 && errors.Is(err, port.ErrVersionConflict) {
			return nil, errPreconditionFailed
		}
		return nil, err
	}
	return newUserOutput(u), nil
}

func (h *userHandler) delete(ctx context.Context, in *userID) (*struct{}, error) {
	if err := h.users.Delete(ctx, in.ID); err != nil {
		return nil, err
	}
	return &struct{}{}, nil
}
