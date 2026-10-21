---
name: add-use-case
description: Use when adding or changing a use case, endpoint, or repository method in this Go hexagonal userapi repo — e.g. "add search by email", "add a field to users", "new endpoint under /api/v1", or changing what an existing endpoint accepts or returns.
---

# Add or change a use case

Work from inside out (domain → port → adapter → HTTP → docs), using TDD at every layer: write the test, **observe the intended failure**, then implement. The requested use case is the user's request that invoked this skill.

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
3. Implement in `memory`, then `postgres`:
   - add a named query to `internal/adapter/outbound/postgres/queries/users.sql` (explicit columns, parameters) and run `make sqlc-generate`; use the `add-migration` skill for schema changes;
   - call the generated method, convert rows with `toDomain`, and map errors with `mapError`;
   - new **writes** use `r.write`, run every query through the `q` it passes, and call `r.appendEvent(ctx, q, ...)` on success, with `outbox_test.go` coverage (see `.claude/rules/messaging.md`).
4. Run both, including PostgreSQL: `TEST_DATABASE_URL=... go test ./internal/adapter/outbound/...`

## 4. Service (`internal/core/service`)

1. Test in `user_service_test.go` using `newSUT()`, including malformed IDs and repository errors.
2. Implement:
   - parse/validate input before repository access (`parseUserID`);
   - wrap errors with `fmt.Errorf("...: %w", err)`;
   - log through `s.logger`.

## 5. HTTP (`internal/adapter/inbound/httpapi`)

1. Test success status/body and every error from step 0 in `user_handler_test.go`, including `Content-Type: application/problem+json`. Use `NewRequestWithContext`.
2. Implement:
   - a Huma operation in `userHandler.register` (`user_handler.go`): `OperationID`, `Tags: ["Users"]`, `DefaultStatus`, `jsonBody[...]` for a body, `Parameters` for values read in `Resolve`, and `problemResponses` listing every error status;
   - input/output structs: typed `path:"..."` string fields; bodies (`bindJSON`), numbers, and headers read in the input's `Resolve` with errors stored for the handler; outputs with `header:"..."` fields and `Body`;
   - the handler: call port → return output, or `nil, err`;
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
| Huma `Body` field, `int` query field, or schema tags (`format`, `maxLength`) | Read in `Resolve` with the existing helpers; Huma's validation changes statuses and `detail` |
| Native `v1.GET(...)` route for an API endpoint | Register a Huma operation so it appears in `/openapi.json` |
| Update postgres but forget memory | Add a shared `repotest` subtest first to check both |
| Write SQL inline in Go, or edit `sqlcgen/` | Add a named query in `queries/*.sql`, then `make sqlc-generate` |
| Call `r.queries` inside `r.write` | Use the `q` argument: `r.queries` runs outside the transaction |
| Change `.golangci.yml` after a depguard error | Fix the incorrect import direction |
| Write `detail` from memory | Call the API and copy the exact response |
