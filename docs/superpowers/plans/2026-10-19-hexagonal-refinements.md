# Tinh chỉnh kiến trúc Hexagonal (mục 1–5) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Siết ranh giới hexagon: logger được inject, lỗi use case nằm ở tầng application, inbound port chỉ nhận primitive, depguard không còn chỗ hở, và composition root mỏng với lifecycle có test tự động.

**Architecture:** Năm task nhỏ, mỗi task một commit. Ba task đầu đổi hợp đồng của core (`service`, `port`, `domain`) rồi sửa mọi nơi gọi. Task 4 chỉ đổi `.golangci.yml`. Task 5 tách `cmd/userapi/main.go` thành package `internal/app` có test. Thứ tự này đảm bảo Task 5 chuyển sang code đã ổn định chữ ký.

**Tech Stack:** Go 1.25, Echo v5, pgx v5, goose v3.27, golangci-lint v2.5 (depguard), PostgreSQL 16 cho integration test.

**Spec:** `docs/superpowers/specs/2026-10-19-hexagonal-refinements.md` (R1–R5)

## Global Constraints

- `go.mod` giữ `go 1.25.0`; không thêm module dependency mới.
- Comment trong code và README viết tiếng Việt, theo giọng văn hiện có. Commit message tiếng Anh, kết thúc bằng hai dòng:
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` và `Claude-Session: https://claude.ai/code/session_01VKqts3hwALRwRHmhykPyh9`.
- Làm việc trên branch `claude/vibrant-keller-k2m8al`.
- Hợp đồng HTTP (status code, JSON, header) không đổi.
- `internal/core/**` (trừ test) chỉ được import thư viện chuẩn và `internal/core/...` (depguard rule `core`).
- **Cổng chất lượng cuối mỗi task** (gọi tắt là "Gate" bên dưới):
  ```bash
  test -z "$(gofmt -l .)" && golangci-lint run ./... && go test -race -count=1 ./...
  # cần PostgreSQL (make db-up, hoặc service container của CI):
  TEST_DATABASE_URL="postgres://userapi:userapi@localhost:5432/userapi?sslmode=disable" \
    go test -race -count=1 ./internal/adapter/outbound/postgres/ ./internal/app/
  ```
  Kết quả mong đợi: không có output từ gofmt, `0 issues.`, mọi package `ok`. Ở Task 1–4, bỏ `./internal/app/` (package này được tạo ở Task 5).

## Review Focus

1. **Database hoàn toàn trống** (chưa từng chạy migrate): hiện `PendingMigrations` trả lỗi `relation "goose_db_version" does not exist`. Người dùng mong thấy thông báo "pending migrations, run `userapi migrate`". Test: Task 5, Step 1 (`TestPendingMigrations_EmptySchema`) và Step 6 (`TestRun_RefusesToStartWithPendingMigrations`).
2. **ID sai định dạng ở PATCH/DELETE** (`not-a-uuid`, hoặc UUID chữ hoa) sau khi bỏ `domain.UserID` khỏi command: phải là 404, không được tới database (Postgres sẽ trả lỗi `22P02`, thành 500). Test: Task 3, Step 1 và Step 6.
3. **PATCH rỗng `{}`** sau khi đổi sang `Email`/`Name *string`: trả user hiện tại, không tăng `version`, không ghi xuống storage. Test: Task 3, Step 1.
4. **Truyền `nil` logger vào `NewUserService`**: không panic khi đường retry ghi log. Test: Task 1, Step 1.
5. **Tắt máy ở các thời điểm bất thường**: cổng đã bị chiếm, ctx đã bị huỷ trước khi `Run` bắt đầu, và tín hiệu thứ hai trong lúc drain. `Run` phải trả về ngay, không treo. Test: Task 5, Step 6. Tín hiệu thứ hai kiểm tra bằng tay ở Task 5, Step 10.

---

### Task 1: Inject logger vào `UserService` (R3)

**Files:**
- Modify: `internal/core/service/user_service.go:23-34` (struct, constructor), `:107` (dòng `slog.InfoContext`)
- Modify (nơi gọi): `cmd/userapi/main.go:134`, `internal/adapter/inbound/httpapi/user_handler_test.go:36`, `internal/adapter/outbound/postgres/user_repository_test.go:58`
- Test: `internal/core/service/user_service_test.go` (`newSUT` dòng 30-33, `newRacingSUT` dòng ~152-157)

**Interfaces:**
- Produces: `func NewUserService(repo port.UserRepository, ids port.IDGenerator, clock port.Clock, logger *slog.Logger) *UserService`. `logger == nil` thì dùng `slog.New(slog.DiscardHandler)`.

- [ ] **Step 1: Viết test (đang fail)**

Đổi `newRacingSUT(races int)` thành `newRacingSUT(logger *slog.Logger)` (tham số `races` hiện không được dùng; mọi nơi gọi đều truyền 0, đổi thành `nil`). `newSUT` truyền `nil`. Thêm:

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
	repo.races = 1 // buộc đi qua đường ghi log
	name := "Renamed"
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}}); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2: Chạy test, xác nhận fail**

Run: `go test ./internal/core/service/`
Expected: lỗi compile `too many arguments in call to service.NewUserService`

- [ ] **Step 3: Thêm field `logger *slog.Logger` và đổi constructor như trong Interfaces; dòng 107 đổi thành `s.logger.InfoContext(...)`, giữ nguyên message và các attribute**

- [ ] **Step 4: Sửa 3 nơi gọi ngoài package**: `main.go` truyền `logger`; hai test helper truyền `nil`.

- [ ] **Step 5: Chạy test**

Run: `go test -race ./internal/core/service/ && go build ./...`
Expected: `ok`, build không lỗi. Kiểm tra thêm: `grep -n 'slog\.\(Info\|Warn\|Error\|Debug\)' internal/core -r --include=*.go | grep -v _test.go` không in ra gì.

- [ ] **Step 6: Gate rồi commit**

```bash
git add -A && git commit -m "Inject the logger into UserService instead of using slog's default"
```

---

### Task 2: Chuyển lỗi kết quả use case sang tầng application (R5)

**Files:**
- Create: `internal/core/port/errors.go`
- Modify: `internal/core/domain/errors.go:9-16` (bỏ `ErrUserNotFound`, `ErrVersionConflict`; sửa comment: domain chỉ giữ lỗi nghiệp vụ)
- Modify: `internal/core/port/port.go:56-60` (comment hợp đồng repository nhắc tới `port.ErrNotFound`, `port.ErrVersionConflict`)
- Modify: `internal/core/service/user_service.go` (dòng 49, 92, 105, 117)
- Modify: `internal/adapter/outbound/memory/user_repository.go` (51, 96, 99, 118), `internal/adapter/outbound/postgres/user_repository.go` (`mapError`, 176-198)
- Modify: `internal/adapter/inbound/httpapi/errors.go:60-65`, `internal/adapter/inbound/httpapi/user_handler.go:153`
- Modify: `README.md` (bảng ánh xạ lỗi: thay tên lỗi)
- Test: `internal/adapter/outbound/repotest/contract.go`, `internal/core/service/user_service_test.go`, `internal/adapter/outbound/postgres/user_repository_test.go`

**Interfaces:**
- Produces (package `port`):
  ```go
  var (
  	ErrNotFound        = errors.New("not found")
  	ErrVersionConflict = errors.New("version conflict")
  )
  ```
- Quy ước: service wrap kèm ngữ cảnh, `fmt.Errorf("user %s: %w", id, port.ErrNotFound)`. Repository trả sentinel trần (ngữ cảnh do service thêm).
- `domain` còn lại: `ErrEmailAlreadyExists`, `FieldError`, `ValidationError`.

- [ ] **Step 1: Viết test (đang fail)**

Trong `repotest/contract.go`, `user_service_test.go` và `postgres/user_repository_test.go`: thay mọi `domain.ErrUserNotFound` bằng `port.ErrNotFound`, mọi `domain.ErrVersionConflict` bằng `port.ErrVersionConflict`. Thêm vào `user_service_test.go`:

```go
func TestGet_NotFoundCarriesContext(t *testing.T) {
	svc, _ := newSUT()
	_, err := svc.Get(ctx, "garbage")
	if !errors.Is(err, port.ErrNotFound) || !strings.Contains(err.Error(), "user garbage") {
		t.Fatalf("want wrapped port.ErrNotFound mentioning the id, got %v", err)
	}
}
```

- [ ] **Step 2: Chạy test, xác nhận fail**

Run: `go vet ./...`
Expected: `undefined: port.ErrNotFound`

- [ ] **Step 3: Tạo `port/errors.go`, xoá hai sentinel khỏi `domain/errors.go`, rồi sửa mọi nơi tham chiếu theo danh sách Files.** Detail trong HTTP: 404 dùng `"resource not found"`, 409 version giữ câu hiện có.

- [ ] **Step 4: Chạy test**

Run: `go test -race ./... && grep -rn 'domain\.Err\(UserNotFound\|VersionConflict\)' --include=*.go .`
Expected: mọi package `ok`; grep không in ra gì.

- [ ] **Step 5: Gate (gồm cả integration test PostgreSQL) rồi commit**

```bash
git add -A && git commit -m "Move use-case outcome errors from domain to the application port"
```

---

### Task 3: Inbound port chỉ nhận primitive (R2)

**Files:**
- Modify: `internal/core/port/port.go:25-31` (`UpdateUserCmd`), `:46-52` (`UserService`)
- Modify: `internal/core/service/user_service.go` (`Get`, `Update`, `Delete`; thêm helper parse ID)
- Modify: `internal/adapter/inbound/httpapi/user_handler.go:106,145-147,162` (bỏ `domain.UserID(...)`, `domain.Changes{...}`)
- Modify: `internal/adapter/outbound/postgres/user_repository_test.go:74,85`
- Modify: `README.md`, mục "Các quyết định thiết kế": thêm một đoạn "Inbound port chỉ nhận primitive" (lý do: mọi transport dùng chung một hợp đồng; validate ID nằm ở core)
- Test: `internal/core/service/user_service_test.go`, `internal/adapter/inbound/httpapi/user_handler_test.go` (`TestErrorResponses`)

**Interfaces:**
- Consumes: `port.ErrNotFound` (Task 2).
- Produces:
  ```go
  type UpdateUserCmd struct {
  	ID              string
  	Email           *string // nil = giữ nguyên
  	Name            *string // nil = giữ nguyên
  	ExpectedVersion int64   // 0 = không kiểm tra (không có If-Match)
  }

  type UserService interface {
  	Register(ctx context.Context, cmd RegisterUserCmd) (domain.User, error) // không đổi
  	Get(ctx context.Context, id string) (domain.User, error)
  	List(ctx context.Context, q ListUsersQuery) (UserPage, error)          // không đổi
  	Update(ctx context.Context, cmd UpdateUserCmd) (domain.User, error)
  	Delete(ctx context.Context, id string) error
  }
  ```
- Trong service: `func parseUserID(raw string) (domain.UserID, error)`. Trả `fmt.Errorf("user %s: %w", raw, port.ErrNotFound)` khi `!domain.UserID(raw).Valid()`. `Get`, `Update`, `Delete` đều gọi hàm này **trước** khi gọi repository.

- [ ] **Step 1: Viết test (đang fail)**

Chuyển các test hiện có sang command mới: `port.UpdateUserCmd{ID: u.ID.String(), Name: &name}`, `svc.Delete(ctx, u.ID.String())`. Thêm:

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

Trong `TestErrorResponses` (HTTP), thêm hai case:
```go
{"patch garbage id", http.MethodPatch, "/api/v1/users/not-a-uuid", `{"name":"x"}`, http.StatusNotFound, 0},
{"delete garbage id", http.MethodDelete, "/api/v1/users/not-a-uuid", "", http.StatusNotFound, 0},
```

- [ ] **Step 2: Chạy test, xác nhận fail**

Run: `go vet ./internal/core/service/`
Expected: lỗi compile, ví dụ `unknown field Name in struct literal of type port.UpdateUserCmd`

- [ ] **Step 3: Đổi `port.go` theo Interfaces; trong service thêm `parseUserID`, dựng `domain.Changes{Email: cmd.Email, Name: cmd.Name}`**

- [ ] **Step 4: Sửa handler**: truyền thẳng `c.Param("id")` và `req.Email`/`req.Name`. Sửa `postgres/user_repository_test.go` cho khớp.

- [ ] **Step 5: Kiểm tra adapter không còn dựng kiểu domain**

Run: `grep -rn 'domain\.\(UserID\|Changes\)' internal/adapter/inbound --include=*.go`
Expected: không in ra gì.

- [ ] **Step 6: Chạy test**

Run: `go test -race ./...`
Expected: mọi package `ok`, gồm hai case HTTP mới.

- [ ] **Step 7: Cập nhật README theo danh sách Files, Gate, rồi commit**

```bash
git add -A && git commit -m "Make inbound port commands primitive-only and validate IDs in the core"
```

---

### Task 4: Bịt chỗ hở của depguard (R4)

**Files:**
- Modify: `.golangci.yml`, phần `linters.settings.depguard.rules`
- Modify: `README.md`, bảng rule depguard trong mục "Kiến trúc"

**Interfaces:**
- Produces các rule sau (đường dẫn module `github.com/example/userapi`):

| Rule | `files` | Thêm vào `deny` |
|---|---|---|
| `outbound-adapters` (đã có) | `**/internal/adapter/outbound/**`, `!$test` | `.../internal/adapter/outbound/memory`, `.../outbound/postgres`, `.../outbound/system`, `.../outbound/repotest`, `.../internal/app` |
| `inbound-adapters` (đã có) | không đổi | `.../internal/app` |
| `platform` (mới) | `**/internal/observability/**`, `**/internal/config/**`, `!$test` | `.../internal/adapter`, `.../internal/core`, `.../internal/app` |

  Mỗi mục `deny` có `desc` một dòng (tiếng Anh, giống các rule hiện có). Rule `core` dùng `allow`, nên đã tự chặn `internal/app`.

- [ ] **Step 1: Tạo ba file vi phạm tạm thời (phải bị bắt)**

```bash
printf 'package system\n\nimport _ "github.com/example/userapi/internal/adapter/outbound/memory"\n' > internal/adapter/outbound/system/zz_violation.go
printf 'package observability\n\nimport _ "github.com/example/userapi/internal/adapter/outbound/memory"\n' > internal/observability/zz_violation.go
printf 'package config\n\nimport _ "github.com/example/userapi/internal/core/domain"\n' > internal/config/zz_violation.go
```

- [ ] **Step 2: Chạy lint, xác nhận các vi phạm hiện chưa bị bắt**

Run: `golangci-lint run ./... | grep -c depguard`
Expected: `0`

- [ ] **Step 3: Thêm rule như bảng Interfaces**

- [ ] **Step 4: Chạy lại lint**

Run: `golangci-lint run ./... | grep depguard`
Expected: đúng 3 dòng, lần lượt nêu list `outbound-adapters`, `platform`, `platform`.

- [ ] **Step 5: Xoá file vi phạm, xác nhận code thật sạch**

Run: `rm internal/adapter/outbound/system/zz_violation.go internal/observability/zz_violation.go internal/config/zz_violation.go && golangci-lint run ./...`
Expected: `0 issues.`

- [ ] **Step 6: Cập nhật bảng depguard trong README, Gate, rồi commit**

```bash
git add -A && git commit -m "Close depguard gaps for platform packages and sibling outbound adapters"
```

---

### Task 5: Tách `internal/app`, composition root mỏng, lifecycle có test (R1)

**Files:**
- Create: `internal/app/app.go`. Chứa `Run`, `Migrate`, và phần chuyển từ `main.go`: chọn storage (dòng 94-131), `readiness`, `errDraining`, dựng `http.Server` (144-156), shutdown (158-176), setup/flush telemetry (81-92).
- Create: `internal/app/app_test.go`
- Create: `internal/adapter/outbound/postgres/pgtest/pgtest.go` (helper dùng chung cho integration test)
- Modify: `internal/adapter/outbound/postgres/migrate.go`, hàm `PendingMigrations`
- Modify: `internal/adapter/outbound/postgres/user_repository_test.go` (thêm test database trống)
- Modify: `cmd/userapi/main.go` (còn tối đa 60 dòng; xoá `shutdownOnSignal`, `readiness`, `errDraining`, `migrate`)
- Modify: `README.md`: cây thư mục (thêm `app/`), bảng Test (thêm dòng "Lifecycle"), câu "Chỉ `main.go` biết tất cả" đổi thành "`cmd` + `internal/app` là composition root"
- Modify: `.github/workflows/ci.yml`: không cần sửa (job test đã chạy `./...` kèm `TEST_DATABASE_URL`); chỉ kiểm tra lại

**Interfaces:**
- Consumes: `service.NewUserService(repo, ids, clock, logger)` (Task 1).
- Produces:
  ```go
  package app

  // Options cho Run. Giá trị zero dùng được cho production.
  type Options struct {
  	Version  string       // ghi vào log khởi động và service.version của OTel
  	Listener net.Listener // nil = net.Listen("tcp", cfg.HTTPAddr); test truyền 127.0.0.1:0
  }

  // Run khởi tạo adapter và phục vụ HTTP cho tới khi ctx bị huỷ. Sau đó: /readyz trả 503,
  // vẫn phục vụ trong cfg.ShutdownDelay, rồi ngừng nhận kết nối và chờ request đang chạy
  // tối đa cfg.ShutdownTimeout. Trả nil khi tắt êm; trả lỗi khi khởi động thất bại hoặc drain quá hạn.
  func Run(ctx context.Context, cfg config.Config, logger *slog.Logger, opts Options) error

  // Migrate áp dụng migration rồi trả về (lệnh `userapi migrate`).
  func Migrate(ctx context.Context, cfg config.Config, logger *slog.Logger) error
  ```
  ```go
  package pgtest

  // FreshSchemaDSN tạo schema rỗng tên "t_<unixnano>" và trả DSN có search_path trỏ vào đó;
  // t.Cleanup sẽ DROP SCHEMA ... CASCADE. Skip test nếu TEST_DATABASE_URL trống.
  func FreshSchemaDSN(t testing.TB) string
  ```
- Thông báo lỗi giữ nguyên: `database has pending migrations: run ` + "`userapi migrate`" + ` first (or set MIGRATE_ON_START=true)`. Lỗi listen được wrap thành `http listen: %w`.
- `main`: `ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`, `defer stop()`, `context.AfterFunc(ctx, stop)` (tín hiệu thứ hai dùng xử lý mặc định, tức là kết thúc tiến trình ngay), rồi gọi `app.Run(ctx, cfg, logger, app.Options{Version: version})` hoặc `app.Migrate(...)`.

- [ ] **Step 1: Viết test database trống (đang fail)**

Trong `pgtest.go`: đọc `TEST_DATABASE_URL`, mở `pgx.Connect`, `CREATE SCHEMA`, rồi trả DSN có thêm query param `search_path=<schema>`. Dùng `net/url` để thêm param, vì DSN có thể chưa có `?`. pgx đưa tham số lạ trong DSN thành runtime param. Trong `postgres/user_repository_test.go`:

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

- [ ] **Step 2: Chạy test, xác nhận fail đúng lý do**

Run: `TEST_DATABASE_URL=... go test -count=1 -run TestPendingMigrations_EmptySchema ./internal/adapter/outbound/postgres/`
Expected: FAIL với `err` chứa `goose_db_version` (`relation ... does not exist`)

- [ ] **Step 3: Sửa `PendingMigrations`**: trước khi gọi goose, chạy `SELECT to_regclass('goose_db_version') IS NOT NULL` trên `pool` (tôn trọng `search_path`); `false` thì trả `(true, nil)`.

- [ ] **Step 4: Chạy lại**

Run: lệnh ở Step 2
Expected: PASS

- [ ] **Step 5: Viết test lifecycle cho `app` (đang fail)**

Trong `app_test.go`, viết helper `startApp(t, env map[string]string) (base string, cancel context.CancelFunc, done <-chan error)`. Helper dựng cfg bằng `config.Load` từ `env` (luôn có `STORAGE=memory`, trừ khi `env` ghi đè), listen `127.0.0.1:0`, chạy `app.Run` trong goroutine, rồi chờ `GET /healthz` = 200 (tối đa 2s). Logger dùng `slog.DiscardHandler`. Các test:

```go
func TestRun_DrainsBeforeClosingListener(t *testing.T) {
	base, cancel, done := startApp(t, map[string]string{"SHUTDOWN_DELAY": "500ms"})
	expectGET(t, base+"/readyz", http.StatusOK)
	cancel()
	time.Sleep(100 * time.Millisecond)
	expectGET(t, base+"/readyz", http.StatusServiceUnavailable) // LB ngừng gửi traffic mới
	expectGET(t, base+"/api/v1/users", http.StatusOK)           // nhưng vẫn phục vụ
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
	dsn := pgtest.FreshSchemaDSN(t) // skip nếu không có TEST_DATABASE_URL
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

`runWithTimeout` chạy `app.Run` và gọi `t.Fatal("Run did not return")` nếu quá hạn. `mustConfig`, `expectGET`, `waitDone` là helper vài dòng trong cùng file. Mọi request trong test (kể cả `http.Get` cuối `TestRun_DrainsBeforeClosingListener`) dùng một `http.Client` có `Transport: &http.Transport{DisableKeepAlives: true}` và `Timeout: time.Second`, để không dùng lại connection cũ, vì như vậy sẽ che mất việc listener đã đóng.

- [ ] **Step 6: Chạy test, xác nhận fail**

Run: `go test ./internal/app/`
Expected: lỗi compile, package `internal/app` chưa có `Run`

- [ ] **Step 7: Tạo `internal/app/app.go` bằng cách chuyển code từ `main.go` theo danh sách Files.** Thay goroutine `shutdownOnSignal` bằng luồng tuần tự trong `Run`: `select` giữa lỗi serve và `<-ctx.Done()`; rồi `draining.Store(true)`; `time.Sleep(cfg.ShutdownDelay)`; rồi `srv.Shutdown`. Khi `opts.Listener` là nil thì tự `net.Listen` trước khi `Serve`, để lỗi cổng trả về đồng bộ.

- [ ] **Step 8: Viết lại `main.go` theo Interfaces (tối đa 60 dòng)**

Run: `wc -l cmd/userapi/main.go && grep -c 'func ' cmd/userapi/main.go`
Expected: ≤ 60 dòng; chỉ còn `func main`.

- [ ] **Step 9: Chạy test**

Run: `go test -race -count=1 ./internal/app/ && TEST_DATABASE_URL=... go test -race -count=1 ./internal/app/ ./internal/adapter/outbound/postgres/`
Expected: `ok` cả hai lần (lần đầu test PostgreSQL bị skip).

- [ ] **Step 10: Kiểm tra bằng tay: tín hiệu thứ hai kết thúc tiến trình ngay**

```bash
go build -o /tmp/userapi ./cmd/userapi
SHUTDOWN_DELAY=10s HTTP_ADDR=:18099 /tmp/userapi & P=$!; sleep 1
kill -TERM $P; sleep 0.3; kill -TERM $P; time wait $P
```
Expected: `wait` trả về trong dưới 1s với exit code 143 (không chờ hết 10s). Chỉ gửi một tín hiệu thì tiến trình chờ khoảng 10s rồi thoát với code 0.

- [ ] **Step 11: Cập nhật README theo danh sách Files, Gate, rồi commit**

```bash
git add -A && git commit -m "Extract app lifecycle into internal/app with tests; keep main thin"
```

---

## Sau khi xong cả 5 task

- [ ] `git push -u origin claude/vibrant-keller-k2m8al`
- [ ] Đối chiếu lại spec R1–R5 với `git log --oneline -5`: mỗi R có đúng một commit tương ứng.
