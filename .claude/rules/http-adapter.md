---
paths:
  - "internal/adapter/inbound/**/*.go"
---

# Adapter HTTP (`internal/adapter/inbound/httpapi`)

- Keep handlers thin: `bindJSON` DTOs (not direct `echo.BindBody`, which also accepts forms/XML) → call `port.UserService` → map response DTOs. No business logic, outbound adapter imports, or `internal/app` imports.
- Pass raw inputs (such as `c.Param("id")`) to inbound ports; do not construct `domain.UserID` or `domain.Changes` in handlers.
- JSON DTOs (`userResponse`, `fieldError`...) belong to adapters; domain types have no JSON tags.
- Handlers normally `return err`; `toProblem` in `errors.go` maps statuses. Handlers create only HTTP-specific errors: malformed body/query (400/413/415) and `If-Match` (412). All errors use `application/problem+json`; 5xx responses hide internal details.
- Changes to status, headers, body, or `detail` change the API contract: update `docs/api.md` (including literal error details) and `api.http` in the same commit.
- New `/api/v1` routes inherit `APIMiddleware`; probes (`/healthz`, `/readyz`) are not logged/traced and bypass authentication middleware.
- Use `httptest.NewRequestWithContext` (`noctx` forbids `httptest.NewRequest`), existing `newAPI`/`newAPIWith`/`expectStatus` helpers, and assert `Content-Type: application/problem+json` on errors.
