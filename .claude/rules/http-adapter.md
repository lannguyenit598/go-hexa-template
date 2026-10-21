---
paths:
  - "internal/adapter/inbound/**/*.go"
---

# Adapter HTTP (`internal/adapter/inbound/httpapi`)

- `/api/v1` endpoints are Huma operations registered in `userHandler.register` (`user_handler.go`) through `register` (`openapi.go`), never native Echo routes; probes stay native Echo routes. Only this package imports Huma (depguard). Do not import `humaecho` (it links Echo v4); `humaAdapter` is the adapter.
- Keep handlers thin: input `Resolve(huma.Context)` reads bodies with `bindJSON` (not Huma's `Body` field or `echo.BindBody`), `If-Match` with `parseIfMatch`, and numbers with `echo.QueryParamOr`, storing errors for the handler to return → call `port.UserService` → return output structs (`header:"..."` fields, `Body`). Huma's own parsing, validation, and `huma.NewError` (process-global; never replace it) would change statuses and `detail`. No business logic, outbound adapter imports, or `internal/app` imports.
- Pass raw inputs (such as the `path:"id"` string) to inbound ports; do not construct `domain.UserID` or `domain.Changes` in handlers. Do not add schema constraints (`format`, `minLength`, `maxLength`) that the domain normalizes or validates.
- JSON DTOs (`userResponse`, `fieldError`...) belong to adapters; domain types have no JSON tags.
- Handlers normally `return nil, err`; `toProblem` in `errors.go` maps statuses. Document every error status in the operation's `problemResponses`. Handlers create only HTTP-specific errors: malformed body/query (400/413/415) and `If-Match` (412). All errors use `application/problem+json`; 5xx responses hide internal details.
- Changes to status, headers, body, or `detail` change the API contract: update `docs/api.md` (including literal error details) and `api.http` in the same commit. `testdata/*.golden` pin the wire format; regenerate with `-update` only for intended changes and review the diff.
- New `/api/v1` routes inherit `APIMiddleware`; probes (`/healthz`, `/readyz`) are not logged/traced and bypass authentication middleware.
- Use `httptest.NewRequestWithContext` (`noctx` forbids `httptest.NewRequest`), existing `newAPI`/`newAPIWith`/`expectStatus` helpers, and assert `Content-Type: application/problem+json` on errors.
