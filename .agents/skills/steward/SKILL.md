---
name: steward
description: Use when driving a pull request of this userapi repo to green or mergeable — a CI job (lint, test, vulncheck, docker) failed, a review comment arrived, the branch has a merge conflict, or a PR is about to be opened.
---

# Get a PR to green

## Before opening a PR

- Base is `main`; compare is the working branch. A reversed PR merges main into the branch without changing main.
- If the branch's previous PR was merged, start a fresh branch from current `origin/main`; do not stack new commits onto merged history.
- Run `make check` locally, with `TEST_DATABASE_URL` when available. State unverified checks in the PR, such as govulncheck when `vuln.go.dev` is unreachable.

## When a CI job fails

Read that job's logs first. Reproduce locally, fix it, rerun `make check`, then push.

| Job | Local reproduction | Common causes and fixes |
|---|---|---|
| `lint` | `golangci-lint run ./...` (version from `ci.yml`), `go mod tidy -diff`, and `make sqlc-check` | **Go version mismatch**: golangci-lint must be built with Go >= `go.mod`; update `ci.yml` and `AGENTS.md`. **New `noctx` errors**: use `httptest.NewRequestWithContext` and `net.ListenConfig{}.Listen(ctx, …)`. **depguard errors**: fix imports, not `.golangci.yml`. **Generated queries are out of date**: run `make sqlc-generate` and commit `sqlcgen/`; never edit it by hand |
| `test` | `TEST_DATABASE_URL=... go test -race -count=1 ./...`, then run `DATABASE_URL=$TEST_DATABASE_URL go run ./cmd/userapi migrate` twice (CI checks idempotency) | **Lifecycle timeout with other packages**: goose advisory lock `4097083626` is database-wide and retries every 5s; allow at least 15s. Reproduce by holding `SELECT pg_advisory_lock(4097083626); SELECT pg_sleep(8);` in psql. **Shared data interference**: postgres tests truncate shared tables; use `pgtest.FreshSchemaDSN` for isolation |
| `vulncheck` | `go run golang.org/x/vuln/cmd/govulncheck@<version from ci.yml> ./...` (requires `vuln.go.dev`; use CI logs if blocked) | Upgrade flagged modules to "Fixed in" versions (`go get <module>@<version>`, `go mod tidy`). If their Go directive raises the minimum, update both READMEs, CI, and golangci-lint |
| `docker` | `docker build .` | The build image (`golang:X-alpine`) must use Go >= the `go.mod` directive |

**Flaky is not a root cause.** Retry a job once only for failures before tests (checkout, installation, lost runner). Repeated failures need diagnosis; never skip or disable tests to make CI green.

## Merge conflict

- Merge `main` into the branch; do not rebase or force-push branches others use.
- For `go.sum` conflicts, take one side and run `go mod tidy`; do not edit manually.
- Resolve `README.md`/`README.vi.md` conflicts consistently across both versions.

## Review comment

- Small local review requests (rename, test, one-function fix): implement, push, and reply briefly in the thread.
- Large requests (API/port changes, multi-file refactors): propose an approach in the thread and wait for the PR owner.
- For reported bugs, write a reproducer first. If it cannot be reproduced, reply with evidence.
