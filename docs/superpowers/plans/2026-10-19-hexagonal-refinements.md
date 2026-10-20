# Hexagonal Architecture Refinements (Items 1–5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Tighten hexagonal boundaries: inject loggers, move use-case errors to application, accept primitive inbound inputs, close depguard gaps, and test lifecycle through a thin composition root.

**Architecture:** Five small tasks, one commit each. First change core contracts and callers, then depguard, then extract internal/app. This order gives Task 5 stable signatures.

**Tech Stack:** Go 1.25, Echo v5, pgx v5, goose v3.27, golangci-lint v2.5 (depguard), PostgreSQL 16 cho integration test.

**Spec:** `docs/superpowers/specs/2026-10-19-hexagonal-refinements.md` (R1–R5)

## Global Constraints

- Keep go.mod at go 1.25.0; add no module dependencies.
- Historical language policy at the time: Vietnamese comments and README. Superseded by the current English-only policy except README.vi.md. Commit messages were English and ended with:
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` and `Claude-Session: https://claude.ai/code/session_01VKqts3hwALRwRHmhykPyh9`.
- Work on branch `claude/vibrant-keller-k2m8al`.
- Preserve the HTTP contract (statuses, JSON, headers).
- Non-test internal/core imports only the standard library and internal/core (depguard core rule).
- **Quality gate after every task**, called Gate below:
  ```bash
  test -z "$(gofmt -l .)" && golangci-lint run ./... && go test -race -count=1 ./...
  # Requires PostgreSQL (make db-up or CI service container).
  TEST_DATABASE_URL="postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable" \
    go test -race -count=1 ./internal/adapter/outbound/postgres/ ./internal/app/
  ```
  Expected: no gofmt output, `0 issues.`, all packages ok. Omit ./internal/app/ in Tasks 1–4; Task 5 creates it.

## Review Focus

1. **Completely empty database:** PendingMigrations currently returns relation goose_db_version does not exist. Expected: pending migrations, run userapi migrate. Tests: Task 5 steps 1 and 6.
2. **Malformed PATCH/DELETE IDs** (not-a-uuid or uppercase UUID) after removing domain.UserID from commands must return 404 without reaching PostgreSQL (22P02 would become 500). Tests: Task 3 steps 1 and 6.
3. **Empty PATCH {}:** return current user without version increments or storage writes. Test: Task 3 step 1.
4. **Nil NewUserService logger:** retry logging must not panic. Test: Task 1 step 1.
5. **Unusual lifecycle timing:** occupied port, pre-canceled context, and second signal during drain. Run must return without hanging. Tests: Task 5 step 6; manual second-signal check in step 10.

---

### Task 1: Inject a logger into UserService (R3)

**Files:**
- Modify: internal/core/service/user_service.go:23-34 (struct/constructor), :107 (slog.InfoContext).
- Modify callers: cmd/userapi/main.go:134, internal/adapter/inbound/httpapi/user_handler_test.go:36, internal/adapter/outbound/postgres/user_repository_test.go:58.
- Test: internal/core/service/user_service_test.go (newSUT lines 30–33, newRacingSUT around 152–157).

**Interfaces:**
- Produces: `func NewUserService(repo port.UserRepository, ids port.IDGenerator, clock port.Clock, logger *slog.Logger) *UserService`. A nil logger uses slog.New(slog.DiscardHandler).

- [ ] **Step 1: Write failing tests**

Change newRacingSUT(races int) to newRacingSUT(logger *slog.Logger); races is unused and every caller passes 0, so replace with nil. newSUT passes nil. Add:

```go
func TestUpdate_LogsRetriesThroughInjectedLogger(t *testing.T) {
    var buf bytes.Buffer
    svc, repo := newRacingSUT(slog.New(slog.NewJSONHandler(&buf, nil)))
    u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
    repo.races = 1
    name := "Renamed"
    if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}}); err != nil {
        t.Fatal(err)
    }
    if !strings.Contains(buf.String(), `"msg":"user update hit a version conflict, retrying"`) ||
        !strings.Contains(buf.String(), `"attempt":1`) {
        t.Fatalf("retry not logged through injected logger: %s", buf.String())
    }
}

func TestNewUserService_NilLoggerIsSafe(t *testing.T) {
    svc, repo := newRacingSUT(nil)
    u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
    repo.races = 1 // Force the logging path.
    name := "Renamed"
    if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}}); err != nil {
        t.Fatal(err)
    }
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run: `go test ./internal/core/service/`
Expected compile error: too many arguments in call to service.NewUserService.

- [ ] **Step 3: Add logger *slog.Logger and update the constructor; replace line 107 with s.logger.InfoContext, preserving message and attributes.**

- [ ] **Step 4: Update external callers:** main passes logger; both test helpers pass nil.

- [ ] **Step 5: Run tests**

Run: `go test -race ./internal/core/service/ && go build ./...`
Expected: ok and successful build. Also verify `grep -n 'slog\.\(Info\|Warn\|Error\|Debug\)' internal/core -r --include=*.go | grep -v _test.go` produces no output.

- [ ] **Step 6: Gate and commit**

```bash
git add -A && git commit -m "Inject the logger into UserService instead of using slog's default"
```

---

### Task 2: Move use-case outcome errors to application (R5)

**Files:**
- Create: `internal/core/port/errors.go`
- Modify internal/core/domain/errors.go:9-16: remove ErrUserNotFound/ErrVersionConflict; document domain business errors only.
- Modify internal/core/port/port.go:56-60: repository contract references port.ErrNotFound and port.ErrVersionConflict.
- Modify internal/core/service/user_service.go lines 49, 92, 105, 117.
- Modify: `internal/adapter/outbound/memory/user_repository.go` (51, 96, 99, 118), `internal/adapter/outbound/postgres/user_repository.go` (`mapError`, 176-198)
- Modify: `internal/adapter/inbound/httpapi/errors.go:60-65`, `internal/adapter/inbound/httpapi/user_handler.go:153`
- Modify README.md error mapping table.
- Test: `internal/adapter/outbound/repotest/contract.go`, `internal/core/service/user_service_test.go`, `internal/adapter/outbound/postgres/user_repository_test.go`

**Interfaces:**
- Produces (package `port`):
  ```go
  var (
    ErrNotFound        = errors.New("not found")
    ErrVersionConflict = errors.New("version conflict")
  )
  ```
- Services wrap context with fmt.Errorf("user %s: %w", id, port.ErrNotFound); repositories return bare sentinels.
- Domain retains ErrEmailAlreadyExists, FieldError, ValidationError.

- [ ] **Step 1: Write failing tests**

Replace domain.ErrUserNotFound with port.ErrNotFound and domain.ErrVersionConflict with port.ErrVersionConflict in repotest/contract.go, user_service_test.go, and postgres/user_repository_test.go. Add to user_service_test.go:

```go
func TestGet_NotFoundCarriesContext(t *testing.T) {
    svc, _ := newSUT()
    _, err := svc.Get(ctx, "garbage")
    if !errors.Is(err, port.ErrNotFound) || !strings.Contains(err.Error(), "user garbage") {
        t.Fatalf("want wrapped port.ErrNotFound mentioning the id, got %v", err)
    }
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run: `go vet ./...`
Expected: `undefined: port.ErrNotFound`

- [ ] **Step 3: Create port/errors.go, remove the two domain sentinels, and update all references.** HTTP 404 detail becomes "resource not found"; retain the existing version-conflict 409 detail.

- [ ] **Step 4: Run tests**

Run: `go test -race ./... && grep -rn 'domain\.Err\(UserNotFound\|VersionConflict\)' --include=*.go .`
Expected: all packages ok; grep produces no output.

- [ ] **Step 5: Gate including PostgreSQL integration tests, then commit**

```bash
git add -A && git commit -m "Move use-case outcome errors from domain to the application port"
```

---

### Task 3: Primitive-only inbound inputs (R2)

**Files:**
- Modify: `internal/core/port/port.go:25-31` (`UpdateUserCmd`), `:46-52` (`UserService`)
- Modify internal/core/service/user_service.go: Get, Update, Delete, and an ID parser helper.
- Modify internal/adapter/inbound/httpapi/user_handler.go:106,145-147,162: remove domain.UserID conversions and domain.Changes literals.
- Modify: `internal/adapter/outbound/postgres/user_repository_test.go:74,85`
- Update README.md design decisions: explain primitive-only inbound inputs, shared transport contracts, and centralized core ID validation.
- Test: `internal/core/service/user_service_test.go`, `internal/adapter/inbound/httpapi/user_handler_test.go` (`TestErrorResponses`)

**Interfaces:**
- Consumes: `port.ErrNotFound` (Task 2).
- Produces:
  ```go
  type UpdateUserCmd struct {
    ID              string
    Email           *string // nil = unchanged
    Name            *string // nil = unchanged
    ExpectedVersion int64   // 0 = no precondition (no If-Match)
  }

  type UserService interface {
    Register(ctx context.Context, cmd RegisterUserCmd) (domain.User, error) // unchanged
    Get(ctx context.Context, id string) (domain.User, error)
    List(ctx context.Context, q ListUsersQuery) (UserPage, error)          // unchanged
    Update(ctx context.Context, cmd UpdateUserCmd) (domain.User, error)
    Delete(ctx context.Context, id string) error
  }
  ```
- Add service parseUserID(raw string) (domain.UserID, error), returning fmt.Errorf("user %s: %w", raw, port.ErrNotFound) for invalid IDs. Get, Update, Delete call it before repositories.

- [ ] **Step 1: Write failing tests**

Update existing tests to port.UpdateUserCmd{ID: u.ID.String(), Name: &name} and svc.Delete(ctx, u.ID.String()). Add:

```go
func TestUpdate_EmptyPatchIsNoOp(t *testing.T) {
    svc, _ := newSUT()
    u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
    got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String()})
    if err != nil || got != u {
        t.Fatalf("empty patch must return the stored user unchanged: %+v %v", got, err)
    }
    if stored, _ := svc.Get(ctx, u.ID.String()); stored.Version != 1 {
        t.Fatalf("version bumped by empty patch: %d", stored.Version)
    }
}

func TestInvalidIDsAreNotFoundForEveryUseCase(t *testing.T) {
    svc, _ := newSUT()
    name := "x"
    for _, id := range []string{"not-a-uuid", "", "0192F3A4-5B6C-7D8E-9F01-23456789ABCD"} {
        if _, err := svc.Get(ctx, id); !errors.Is(err, port.ErrNotFound) {
            t.Errorf("Get(%q) = %v", id, err)
        }
        if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: id, Name: &name}); !errors.Is(err, port.ErrNotFound) {
            t.Errorf("Update(%q) = %v", id, err)
        }
        if err := svc.Delete(ctx, id); !errors.Is(err, port.ErrNotFound) {
            t.Errorf("Delete(%q) = %v", id, err)
        }
    }
}
```

Add two HTTP TestErrorResponses cases:
```go
{"patch garbage id", http.MethodPatch, "/api/v1/users/not-a-uuid", `{"name":"x"}`, http.StatusNotFound, 0},
{"delete garbage id", http.MethodDelete, "/api/v1/users/not-a-uuid", "", http.StatusNotFound, 0},
```

- [ ] **Step 2: Run tests and confirm failure**

Run: `go vet ./internal/core/service/`
Expected compile errors, such as unknown field Name in port.UpdateUserCmd.

- [ ] **Step 3: Update port.go; add parseUserID and construct domain.Changes{Email: cmd.Email, Name: cmd.Name} in service.**

- [ ] **Step 4: Update handlers** to pass c.Param("id"), req.Email/Name directly; adjust postgres/user_repository_test.go.

- [ ] **Step 5: Verify adapters no longer construct domain types**

Run: `grep -rn 'domain\.\(UserID\|Changes\)' internal/adapter/inbound --include=*.go`
Expected: no output.

- [ ] **Step 6: Run tests**

Run: `go test -race ./...`
Expected: all packages ok, including both new HTTP cases.

- [ ] **Step 7: Update README, Gate, and commit**

```bash
git add -A && git commit -m "Make inbound port commands primitive-only and validate IDs in the core"
```

---

### Task 4: Close depguard gaps (R4)

**Files:**
- Modify .golangci.yml linters.settings.depguard.rules.
- Update README.md architecture depguard table.

**Interfaces:**
- Produce these rules for module github.com/example/userapi:

| Rule | files | Add to deny |
|---|---|---|
| outbound-adapters (existing) | **/internal/adapter/outbound/**, !$test | .../internal/adapter/outbound/memory, .../outbound/postgres, .../outbound/system, .../outbound/repotest, .../internal/app |
| inbound-adapters (existing) | unchanged | .../internal/app |
| platform (new) | **/internal/observability/**, **/internal/config/**, !$test | .../internal/adapter, .../internal/core, .../internal/app |

  Each deny entry has a one-line English desc, like existing rules. The core allowlist already rejects internal/app.

- [ ] **Step 1: Add three temporary violations**

```bash
printf 'package system\n\nimport _ "github.com/example/userapi/internal/adapter/outbound/memory"\n' > internal/adapter/outbound/system/zz_violation.go
printf 'package observability\n\nimport _ "github.com/example/userapi/internal/adapter/outbound/memory"\n' > internal/observability/zz_violation.go
printf 'package config\n\nimport _ "github.com/example/userapi/internal/core/domain"\n' > internal/config/zz_violation.go
```

- [ ] **Step 2: Run lint and confirm the violations are not yet caught**

Run: `golangci-lint run ./... | grep -c depguard`
Expected: `0`

- [ ] **Step 3: Add the specified rules**

- [ ] **Step 4: Rerun lint**

Run: `golangci-lint run ./... | grep depguard`
Expected: exactly three violations listing outbound-adapters, platform, platform.

- [ ] **Step 5: Remove temporary violations and confirm real code passes**

Run: `rm internal/adapter/outbound/system/zz_violation.go internal/observability/zz_violation.go internal/config/zz_violation.go && golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 6: Update README rule table, Gate, and commit**

```bash
git add -A && git commit -m "Close depguard gaps for platform packages and sibling outbound adapters"
```

---

### Task 5: Extract internal/app and test lifecycle (R1)

**Files:**
- Create internal/app/app.go with Run, Migrate, and moved main.go logic: storage selection (94–131), readiness/errDraining, HTTP server construction (144–156), shutdown (158–176), telemetry setup/flush (81–92).
- Create: `internal/app/app_test.go`
- Create internal/adapter/outbound/postgres/pgtest/pgtest.go for shared integration helpers.
- Modify PendingMigrations in internal/adapter/outbound/postgres/migrate.go.
- Add an empty-database test in postgres/user_repository_test.go.
- Reduce cmd/userapi/main.go to at most 60 lines; remove shutdownOnSignal, readiness, errDraining, migrate.
- Update README tree, test table (Lifecycle), and composition-root description to cmd + internal/app.
- Verify .github/workflows/ci.yml; no changes needed because tests already run ./... with TEST_DATABASE_URL.

**Interfaces:**
- Consumes: `service.NewUserService(repo, ids, clock, logger)` (Task 1).
- Produces:
  ```go
  package app

  // Options configures Run. The zero value is suitable for production.
  type Options struct {
    Version  string       // Startup log and OTel service.version.
    Listener net.Listener // nil = net.Listen("tcp", cfg.HTTPAddr); tests supply 127.0.0.1:0.
  }

  // Run creates adapters and serves HTTP until ctx is canceled. Then /readyz returns 503,
  // serving continues for cfg.ShutdownDelay, then connections stop and active requests drain
  // for cfg.ShutdownTimeout. Returns nil on clean shutdown, or startup/drain errors.
  func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error

  // Migrate applies migrations and returns (userapi migrate).
  func Migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error
  ```
  ```go
  package pgtest

  // FreshSchemaDSN creates t_<unixnano> and returns a DSN with search_path targeting it.
  // t.Cleanup drops the schema with CASCADE; skip without TEST_DATABASE_URL.
  func FreshSchemaDSN(t testing.TB) string
  ```
- Preserve error text: database has pending migrations: run `userapi migrate` first (or set MIGRATE_ON_START=true). Wrap listen errors with http listen: %w.
- main uses signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM), defer stop(), and context.AfterFunc(ctx, stop) so the second signal terminates immediately, then calls app.Run or app.Migrate.

- [ ] **Step 1: Write a failing empty-database test**

In pgtest.go read TEST_DATABASE_URL, connect with pgx, CREATE SCHEMA, and return a DSN with search_path. Use net/url because the DSN may lack '?'. pgx maps unknown DSN parameters to runtime parameters. In postgres/user_repository_test.go:

```go
func TestPendingMigrations_EmptySchema(t *testing.T) {
    dsn := pgtest.FreshSchemaDSN(t)
    ctx := context.Background()
    pool, err := postgres.Connect(ctx, dsn, postgres.PoolOptions{MaxConns: 2})
    if err != nil {
        t.Fatal(err)
    }
    t.Cleanup(pool.Close)

    pending, err := postgres.PendingMigrations(ctx, pool)
    if err != nil || !pending {
        t.Fatalf("empty database: pending=%v err=%v, want pending=true err=nil", pending, err)
    }
    if err := postgres.Migrate(ctx, dsn, slog.New(slog.DiscardHandler)); err != nil {
        t.Fatal(err)
    }
    if pending, err := postgres.PendingMigrations(ctx, pool); err != nil || pending {
        t.Fatalf("after migrate: pending=%v err=%v", pending, err)
    }
}
```

- [ ] **Step 2: Run and confirm the intended failure**

Run: `TEST_DATABASE_URL=... go test -count=1 -run TestPendingMigrations_EmptySchema ./internal/adapter/outbound/postgres/`
Expected: FAIL with goose_db_version in the error (relation does not exist).

- [ ] **Step 3: Fix PendingMigrations:** before goose, run SELECT to_regclass('goose_db_version') IS NOT NULL through the pool, respecting search_path; false returns (true, nil).

- [ ] **Step 4: Rerun**

Run the command from Step 2.
Expected: PASS

- [ ] **Step 5: Write failing app lifecycle tests**

Add startApp(t, env map[string]string) (base string, cancel context.CancelFunc, done <-chan error). Load config from env with default STORAGE=memory, listen on 127.0.0.1:0, run app.Run in a goroutine, and await /healthz=200 for up to 2s. Use slog.DiscardHandler. Tests:

```go
func TestRun_DrainsBeforeClosingListener(t *testing.T) {
    base, cancel, done := startApp(t, map[string]string{"SHUTDOWN_DELAY": "500ms"})
    expectGET(t, base+"/readyz", http.StatusOK)
    cancel()
    time.Sleep(100 * time.Millisecond)
    expectGET(t, base+"/readyz", http.StatusServiceUnavailable) // LB stops new traffic.
    expectGET(t, base+"/api/v1/users", http.StatusOK)           // Still serving.
    if err := waitDone(t, done, 3*time.Second); err != nil {
        t.Fatalf("Run = %v, want nil", err)
    }
    if _, err := http.Get(base + "/healthz"); err == nil {
        t.Fatal("listener still open after shutdown")
    }
}

func TestRun_FailsFastWhenAddressInUse(t *testing.T) {
    busy, _ := net.Listen("tcp", "127.0.0.1:0")
    t.Cleanup(func() { _ = busy.Close() })
    cfg := mustConfig(t, map[string]string{"HTTP_ADDR": busy.Addr().String()})
    err := runWithTimeout(t, context.Background(), cfg, app.Options{}, 2*time.Second)
    if err == nil || !strings.Contains(err.Error(), "address already in use") {
        t.Fatalf("Run = %v, want listen error", err)
    }
}

func TestRun_CanceledBeforeStartReturnsPromptly(t *testing.T) {
    ctx, cancel := context.WithCancel(context.Background())
    cancel()
    ln, _ := net.Listen("tcp", "127.0.0.1:0")
    if err := runWithTimeout(t, ctx, mustConfig(t, nil), app.Options{Listener: ln}, time.Second); err != nil {
        t.Fatalf("Run = %v, want nil", err)
    }
}

func TestRun_RefusesToStartWithPendingMigrations(t *testing.T) {
    dsn := pgtest.FreshSchemaDSN(t) // Skip without TEST_DATABASE_URL.
    env := map[string]string{"STORAGE": "postgres", "DATABASE_URL": dsn}
    ln, _ := net.Listen("tcp", "127.0.0.1:0")
    err := runWithTimeout(t, context.Background(), mustConfig(t, env), app.Options{Listener: ln}, 5*time.Second)
    if err == nil || !strings.Contains(err.Error(), "pending migrations") {
        t.Fatalf("Run = %v, want pending migrations error", err)
    }

    env["MIGRATE_ON_START"] = "true"
    base, cancel, done := startApp(t, env)
    expectGET(t, base+"/api/v1/users", http.StatusOK)
    cancel()
    if err := waitDone(t, done, 3*time.Second); err != nil {
        t.Fatal(err)
    }
}
```

runWithTimeout runs app.Run and fails if it does not return on time. mustConfig, expectGET, waitDone are small local helpers. All requests use a client with DisableKeepAlives: true and Timeout: time.Second, including the final GET in TestRun_DrainsBeforeClosingListener, so connection reuse cannot hide listener closure.

- [ ] **Step 6: Run tests and confirm failure**

Run: `go test ./internal/app/`
Expected compile error: internal/app has no Run yet.

- [ ] **Step 7: Extract app.go from main.go.** Replace shutdownOnSignal with sequential Run logic: select serve error or ctx.Done, set draining, wait ShutdownDelay, then srv.Shutdown. With nil opts.Listener, listen before Serve so port errors return synchronously.

- [ ] **Step 8: Rewrite main.go using the specified interface (at most 60 lines)**

Run: `wc -l cmd/userapi/main.go && grep -c 'func ' cmd/userapi/main.go`
Expected: at most 60 lines and only func main.

- [ ] **Step 9: Run tests**

Run: `go test -race -count=1 ./internal/app/ && TEST_DATABASE_URL=... go test -race -count=1 ./internal/app/ ./internal/adapter/outbound/postgres/`
Expected: ok both times (PostgreSQL tests skip on the first run).

- [ ] **Step 10: Manually verify the second signal exits immediately**

```bash
go build -o /tmp/userapi ./cmd/userapi
SHUTDOWN_DELAY=10s HTTP_ADDR=:18099 /tmp/userapi & P=$!; sleep 1
kill -TERM $P; sleep 0.3; kill -TERM $P; time wait $P
```
Expected: wait returns in under 1s with exit code 143, without waiting 10s. One signal waits about 10s and exits 0.

- [ ] **Step 11: Update README, Gate, and commit**

```bash
git add -A && git commit -m "Extract app lifecycle into internal/app with tests; keep main thin"
```

---

## After all five tasks

- [ ] `git push -u origin claude/vibrant-keller-k2m8al`
- [ ] Compare R1–R5 against git log --oneline -5: one commit per requirement.
