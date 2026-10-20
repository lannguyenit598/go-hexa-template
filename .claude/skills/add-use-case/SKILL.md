---
name: add-use-case
description: Use when adding or changing a use case, endpoint, or repository method in this Go hexagonal userapi repo — e.g. "add search by email", "add a field to users", "new endpoint under /api/v1", or changing what an existing endpoint accepts or returns.
argument-hint: "[use case description]"
---

# Add or change a use case

Work from inside out (domain → port → adapter → HTTP → docs), using TDD at every layer: write the test, **observe the intended failure**, then implement. Requested use case: $ARGUMENTS

## 0. Define the contract before coding

Write down the following in the response or PR and follow it:
- endpoint and method;
- request fields, primitive types, and whether they are required;
- success status and response body;
- every error and its core-error-to-status mapping.

Compare against `docs/api.md`. Explicitly flag existing status, `detail`, or field changes as breaking changes.

## 1. Domain (`internal/core/domain`), only for new business rules

- Put new invariants in constructors/methods or a value object; use `ValidationError` with JSON field names.
- Add `*_test.go` tests in package `domain_test`.

## 2. Port (`internal/core/port/port.go`)

- Add a `UserService` method. Use primitive inputs; group multiple parameters into `XxxCmd`/`XxxQuery`.
- Add storage methods to `UserRepository` as needed and **document error contracts** in interface comments (`ErrNotFound`, `ErrVersionConflict`, `domain.ErrEmailAlreadyExists`).

## 3. Repository: contract tests first

1. Add a subtest to `internal/adapter/outbound/repotest/contract.go`.
2. Observe failure: `go test ./internal/adapter/outbound/memory/`.
3. Implement in `memory`, then `postgres` (parameterized SQL, `mapError`; use `/add-migration` for schema changes). New postgres **writes** use `r.write` and call `r.appendEvent` on success, with `outbox_test.go` coverage (see `.claude/rules/messaging.md`).
4. Run both, including PostgreSQL: `TEST_DATABASE_URL=... go test ./internal/adapter/outbound/...`

## 4. Service (`internal/core/service`)

1. Test in `user_service_test.go` using `newSUT()`, including malformed IDs and repository errors.
2. Implement:
   - parse/validate input before repository access (`parseUserID`);
   - wrap errors with `fmt.Errorf("...: %w", err)`;
   - log qua `s.logger`.

## 5. HTTP (`internal/adapter/inbound/httpapi`)

1. Test success status/body and every error from step 0 in `user_handler_test.go`, including `Content-Type: application/problem+json`. Use `NewRequestWithContext`.
2. Implement:
   - route trong `server.go`;
   - handlers in `user_handler.go`: bind DTO → call port → map DTO;
   - map new core errors in `toProblem` in `errors.go`.

## 6. Documentation (same commit)

- `docs/api.md`: endpoint, request/response, and error table with literal `detail`.
- `api.http`: an example request.
- Update `docs/architecture.md` for new ports/adapters.
- Update `docs/events.md` for new event types or changed fields.
- Verify against a running server: `go run ./cmd/userapi`; exercise every step-0 case with `curl`, comparing status and body against the documentation.

## 7. Complete

`TEST_DATABASE_URL=... make check` must pass. Without PostgreSQL, still run `make check` and state that PostgreSQL tests were skipped.

## Common mistakes

| Mistake | Correct approach |
|---|---|
| Port command accepts `domain.UserID` | Accept `string`; service calls `parseUserID` |
| Add `ErrXxxNotFound` | Use `port.ErrNotFound` and wrap with context |
| Handler validates business rules | Move rules to domain/service; handler only binds and maps |
| Update postgres but forget memory | Add a shared `repotest` subtest first to check both |
| Change `.golangci.yml` after a depguard error | Fix the incorrect import direction |
| Write `detail` from memory | Call the API and copy the exact response |
