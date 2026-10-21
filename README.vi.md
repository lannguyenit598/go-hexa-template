# userapi: Go + Echo v5 + Hexagonal Architecture

[English](README.md) | **Tiếng Việt**

Repo mẫu cho một REST API quản lý user, tổ chức theo kiến trúc Hexagonal (Ports & Adapters) và sẵn sàng cho production. Gồm:
- domain có kiểm tra dữ liệu;
- hai adapter lưu trữ hoán đổi được (in-memory và PostgreSQL), cùng chạy một bộ contract test;
- optimistic locking kèm `ETag`/`If-Match`;
- lỗi theo chuẩn RFC 9457;
- migration bằng goose và truy vấn SQL có kiểu do sqlc sinh ra;
- graceful shutdown cho Kubernetes;
- log có cấu trúc và OpenTelemetry;
- phát sự kiện lên Kafka qua transactional outbox (tắt mặc định), không sửa core;
- quy tắc phụ thuộc được CI kiểm tra tự động.

Domain `user` là ví dụ minh hoạ đủ mọi tầng, không bắt buộc giữ. Thêm domain của bạn bằng `/add-use-case`, và xoá `user` khi đã có domain thật thay thế.

| Thành phần | Phiên bản |
|---|---|
| Go | 1.26 trở lên (bản vá bảo mật HTTP/2 của `golang.org/x/net` yêu cầu Go 1.26) |
| Echo | `github.com/labstack/echo/v5` v5.4.0 |
| Huma (operation có kiểu, OpenAPI 3.1) | `github.com/danielgtaylor/huma/v2` v2.39.1 |
| PostgreSQL driver | `github.com/jackc/pgx/v5` v5.11.0 |
| Migration | `github.com/pressly/goose/v3` v3.28.0 |
| Dependency injection và lifecycle (chỉ `internal/app`) | `go.uber.org/fx` v1.24.0 |
| Truy vấn SQL có kiểu (công cụ sinh mã, chạy qua `make`; không phải dependency của module) | sqlc v1.31.1 |
| OpenTelemetry | `go.opentelemetry.io/otel` v1.47.0 |
| Kafka client | `github.com/twmb/franz-go` v1.22.1 |
| PostgreSQL | 18 |

## Tài liệu

| Bạn là | Đọc |
|---|---|
| Người gọi API | [docs/api.md](docs/api.md): endpoint, schema, header, phân trang, `ETag`/`If-Match`, bảng lỗi đầy đủ |
| Người triển khai, vận hành | [docs/operations.md](docs/operations.md): cấu hình, Kubernetes, migration và phân quyền database, observability, xử lý sự cố |
| Người phát triển trên template | [docs/architecture.md](docs/architecture.md): quy tắc phụ thuộc, luồng request, quyết định thiết kế, cách mở rộng |
| Service tiêu thụ sự kiện | [docs/events.md](docs/events.md): loại sự kiện, định dạng message, cam kết về giao nhận và thứ tự |
| Bắt đầu dự án mới từ template | [docs/new-project.md](docs/new-project.md): đổi tên, bỏ phần không dùng, thiết lập GitHub, các quyết định trước khi lên production |

## Yêu cầu

- Go 1.26 trở lên
- Docker có Compose v2 (`docker compose`), cho các lệnh cần PostgreSQL
- [golangci-lint](https://golangci-lint.run/) v2.14 trở lên (bản build bằng Go 1.26), cho `make lint`. CI dùng v2.14.0

## Chạy nhanh

```bash
# 1. Không cần database: dữ liệu nằm trong RAM
make run                      # hoặc: go run ./cmd/userapi

# 2. Với PostgreSQL: khởi động DB bằng docker compose, chạy migration, rồi chạy API
make run-pg

# 3. Toàn bộ stack trong container (DB, migration, API)
make up
```

Thử API:

```bash
curl -i -X POST localhost:8080/api/v1/users \
  -H 'Content-Type: application/json' \
  -d '{"email":"Lan@Example.com","name":"Lan  Nguyen"}'
# 201 Created, Location: /api/v1/users/<uuid>, ETag: "1"
# email được chuẩn hoá thành chữ thường, khoảng trắng thừa trong name bị gộp

curl 'localhost:8080/api/v1/users?limit=10'
```

File [`api.http`](api.http) có sẵn request mẫu cho REST Client (VS Code) hoặc HTTP Client (JetBrains).

> Template **không có xác thực**. Đọc [operations.md › Authentication](docs/operations.md#authentication) trước khi đưa lên môi trường thật.

## Kiến trúc

Core (`domain`, `port`, `service`) không phụ thuộc framework hay database. Adapter inbound (HTTP) gọi vào core qua inbound port; adapter outbound (PostgreSQL, in-memory, đồng hồ, sinh ID) hiện thực các outbound port. Chỉ composition root (`cmd/userapi` + `internal/app`) biết tất cả và lắp chúng lại. Chi tiết, sơ đồ và lý do của từng quyết định: [docs/architecture.md](docs/architecture.md).

```
cmd/userapi/main.go          điểm vào: config, logger, tín hiệu, chọn lệnh (chạy API / migrate / relay)
internal/
  app/                       composition root: lắp adapter vào core, readiness, drain khi tắt, timeout server
  core/
    domain/                  entity User, value object Email, ValidationError, ErrEmailAlreadyExists
    port/                    interface inbound (UserService) và outbound (UserRepository, IDGenerator, Clock);
                             lỗi kết quả use case (ErrNotFound, ErrVersionConflict)
    service/                 use case: Register, Get, List, Update (PATCH), Delete
  adapter/
    inbound/httpapi/         Echo v5 + Huma: operation, DTO, middleware, OpenAPI, ánh xạ lỗi sang problem+json
    outbound/memory/         repository trong RAM (map + RWMutex)
    outbound/postgres/       repository pgx + outbox; migrations/ (SQL nhúng, chạy bằng goose);
                             queries/ (SQL) được sqlc biên dịch thành sqlcgen/ (mã sinh ra, có commit)
    outbound/postgres/pgtest/  helper integration test: schema PostgreSQL rỗng, riêng cho mỗi test
    outbound/repotest/       contract test dùng chung cho mọi repository
    outbound/system/         sinh UUIDv7, đồng hồ UTC
    outbound/kafka/          gửi sự kiện từ outbox lên Kafka (franz-go)
  config/                    đọc cấu hình từ biến môi trường
  observability/             slog handler theo ngữ cảnh request, khởi tạo OpenTelemetry
  outbox/                    vòng lặp relay chuyển sự kiện từ outbox lên broker (Source/Sink)
docs/                        tài liệu API, vận hành, kiến trúc
.github/workflows/ci.yml     CI: lint, test (kèm PostgreSQL), migration, govulncheck, Docker build
api.http                     request mẫu
```

## Phát triển

```bash
make help               # liệt kê mọi lệnh
make test               # unit + HTTP + lifecycle, không cần database
make test-integration   # thêm test trên PostgreSQL thật (TRUNCATE bảng users và outbox trong DB của docker compose!)
make migrate DB_URL=postgres://...   # mọi lệnh make cần PostgreSQL bỏ qua Docker khi truyền DB_URL
make lint               # golangci-lint, gồm depguard kiểm tra quy tắc phụ thuộc
make sqlc-generate      # sinh lại truy vấn có kiểu sau khi sửa queries/ hoặc migrations/ (make check báo lỗi nếu mã cũ)
make build              # build binary vào ./bin, kèm version từ git
```

| Tầng | Ở đâu | Kiểm tra gì |
|---|---|---|
| Domain | `core/domain/*_test.go` | Chuẩn hoá, validate, `Apply` không làm thay đổi bản gốc |
| Service | `core/service/*_test.go` | Use case, ID không hợp lệ, retry khi xung đột version; clock và ID giả để kết quả tất định |
| Contract | `adapter/outbound/repotest` | **Cùng một bộ test** chạy cho memory và postgres, gồm cả compare-and-set theo version |
| PostgreSQL | `adapter/outbound/postgres/*_test.go` | 20 goroutine cùng update một user: không lần ghi nào bị mất; database trống được nhận diện là còn migration chưa chạy |
| HTTP | `adapter/inbound/httpapi/*_test.go` | CRUD, problem+json, ETag/If-Match, 499, IP qua proxy tin cậy, `X-Request-Id`, probe không bị log/trace |
| Lifecycle | `app/*_test.go` | Drain khi tắt, cổng bị chiếm, database còn migration chưa chạy; sự kiện do API ghi tới được Kafka qua relay |
| Sự kiện | `outbox/*_test.go`, `adapter/outbound/{postgres,kafka}/*_test.go` | Mỗi lần ghi thành công sinh đúng một sự kiện, cùng transaction; các relay không lấy trùng message; thử lại sau khi broker lỗi; test Kafka dùng cluster in-memory `kfake` |
| Observability | `observability/*_test.go` | `request_id`/`trace_id` được gắn vào log từ ctx |
| Config | `config/*_test.go` | Giá trị mặc định và lỗi cấu hình |

Test cần PostgreSQL chỉ chạy khi có `TEST_DATABASE_URL`, nếu không sẽ được skip. CI luôn đặt biến này với một PostgreSQL service container.

## Bắt đầu service mới

Trên bản clone mới của template, đổi module path và tên service bằng một lệnh, rồi làm tiếp theo [docs/new-project.md](docs/new-project.md):

```bash
scripts/rename.sh orders github.com/acme/orders
```

Script chỉ có trong template: khi chạy, nó tự xoá chính nó và mục này.

## Giấy phép

[MIT](LICENSE)
