---
paths:
  - "**/*_test.go"
  - "internal/adapter/outbound/repotest/**"
  - "internal/adapter/outbound/postgres/pgtest/**"
---

# Writing tests

- TDD: run a test and observe it **fail for the intended reason** before implementation. An immediately passing test may not check the intended behavior.
- Name tests by behavior (`TestUpdate_EmptyPatchIsNoOp`); use tables for multiple inputs with the same expectation.
- Never call `t.Fatal`/`t.FailNow` outside the test goroutine: prepare fallible resources (config, listeners...) before `go func()` and return errors through channels.
- Do not use `time.Sleep` to await a condition; poll with a deadline (see `startApp` in `internal/app/app_test.go`). Allow for underlying retries: goose advisory locks retry every 5s, so migration tests need at least 15s.
- HTTP clients testing connection closure must disable keep-alive (`DisableKeepAlives: true`); create requests with `NewRequestWithContext`.
- PostgreSQL tests:
  - read `TEST_DATABASE_URL` and skip when absent;
  - use `pgtest.FreshSchemaDSN(t)` for isolated empty schemas and migrations;
  - tests in package `postgres` share and `TRUNCATE` the users table; do not rely on data from other tests.
- `go test ./...` runs packages **concurrently** against the same database; do not rely on package order.
