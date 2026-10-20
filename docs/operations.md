# Vận hành

Tài liệu cho người triển khai và vận hành `userapi`: cấu hình, triển khai trên Kubernetes, migration, observability và xử lý sự cố.

## Cấu hình

Mọi cấu hình đọc từ biến môi trường. Cấu hình sai làm tiến trình dừng ngay khi khởi động (exit code 1), và mọi lỗi được báo cùng một lúc trên một dòng. Dòng này là text ra **stderr** (`ERROR invalid configuration error=...`), không phải JSON, vì logger JSON chỉ được dựng sau khi đọc xong cấu hình. Mẫu đầy đủ: [`.env.example`](../.env.example).

| Biến | Mặc định | Ý nghĩa | Ràng buộc |
|---|---|---|---|
| `HTTP_ADDR` | `:8080` | Địa chỉ lắng nghe | |
| `STORAGE` | `memory` | `memory` hoặc `postgres` | Không phân biệt hoa thường |
| `DATABASE_URL` | (trống) | URL PostgreSQL | Bắt buộc khi `STORAGE=postgres` và cho lệnh `migrate` |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` | Không phân biệt hoa thường; nhận cả dạng `warn+1` của `slog` |
| `REQUEST_TIMEOUT` | `10s` | Timeout của context cho mỗi request | `> 0` |
| `HTTP_IDLE_TIMEOUT` | `75s` | Thời gian giữ keep-alive connection rảnh | `> 0`; phải lớn hơn idle timeout của LB |
| `SHUTDOWN_DELAY` | `0s` | Thời gian vẫn phục vụ sau SIGTERM trước khi đóng listener | `≥ 0`; Kubernetes: 5s–10s |
| `SHUTDOWN_TIMEOUT` | `10s` | Thời gian chờ request đang chạy, tính sau `SHUTDOWN_DELAY` | `≥ 0` |
| `TRUSTED_PROXIES` | (trống) | CIDR của LB/ingress, phân tách bằng dấu phẩy | CIDR hợp lệ |
| `READINESS_CHECK_DB` | `false` | `/readyz` có ping database (timeout 500ms) hay không | Giá trị boolean của Go: `true`/`false`, `1`/`0`, `t`/`f`... |
| `DB_MAX_CONNS` | `10` | Số kết nối tối đa **mỗi pod**. Luôn được áp dụng, ghi đè `pool_max_conns` nếu có trong `DATABASE_URL` | `≥ 1` |
| `DB_MIN_CONNS` | `0` | Số kết nối giữ sẵn. `0` = dùng giá trị của URL (`pool_min_conns`) hoặc mặc định của pgx | `0 ≤ DB_MIN_CONNS ≤ DB_MAX_CONNS` |
| `DB_MAX_CONN_LIFETIME` | `30m` | Tuổi thọ tối đa của một kết nối (giúp cân bằng lại sau failover) | `≤ 0` = mặc định của pgx |
| `DB_MAX_CONN_IDLE_TIME` | `5m` | Đóng kết nối rảnh quá lâu | `≤ 0` = mặc định của pgx |
| `DB_STATEMENT_TIMEOUT` | `5s` | `statement_timeout` phía PostgreSQL cho kết nối của app. Vượt quá thì request trả 503 | `≤ 0` = không đặt (dùng cấu hình của server) |
| `MIGRATE_ON_START` | `false` | Chạy migration khi app khởi động. Tiện cho dev; production dùng `userapi migrate` | |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (trống) | Endpoint OTLP/HTTP. OpenTelemetry bật khi biến này **hoặc** `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` có giá trị (và `OTEL_SDK_DISABLED` khác `true`). Chỉ đặt endpoint cho trace thì metric vẫn được gửi tới endpoint mặc định của SDK (`localhost:4318`) | Các biến `OTEL_*` chuẩn khác cũng được hỗ trợ (`OTEL_SERVICE_NAME`, `OTEL_TRACES_SAMPLER`, `OTEL_SDK_DISABLED`…) |

Giá trị thời gian dùng cú pháp Go: `500ms`, `10s`, `5m`, `1h30m`.

## Triển khai trên Kubernetes

### Shutdown không làm rơi request

Khi nhận SIGTERM, app:
1. đánh dấu `/readyz` là 503, nhưng **vẫn phục vụ bình thường** trong `SHUTDOWN_DELAY`;
2. đóng listener và chờ tối đa `SHUTDOWN_TIMEOUT` cho request đang chạy;
3. đóng pool database, flush trace/metric (tối đa 5s, chỉ khi bật OpenTelemetry), rồi thoát với code 0.

Nếu hết `SHUTDOWN_TIMEOUT` mà vẫn còn request chạy, app huỷ các request đó (log ghi status 499, client thấy kết nối bị đóng), đóng mọi kết nối, rồi thoát với **code 1** và log `userapi stopped with error` kèm `graceful shutdown: context deadline exceeded`. Exit code khác 0 là tín hiệu để điều chỉnh `SHUTDOWN_TIMEOUT` hoặc tìm request chạy quá lâu.

Lý do phải chờ: Kubernetes gỡ pod khỏi Endpoints và cập nhật kube-proxy/ingress *bất đồng bộ* với SIGTERM. Đóng listener ngay sẽ gây 502 ở mỗi lần rolling deploy. Image distroless không có lệnh `sleep` cho `preStop` kiểu exec; Kubernetes 1.30+ có action `preStop.sleep` dựng sẵn, nhưng chờ trong app thì chạy được trên mọi nền tảng.

Đặt `terminationGracePeriodSeconds` lớn hơn `SHUTDOWN_DELAY + SHUTDOWN_TIMEOUT + 5s` (5s cho việc flush telemetry; ví dụ 5s + 10s + 5s, đặt 30s). Tín hiệu thứ hai (Ctrl+C lần nữa khi chạy local) kết thúc tiến trình ngay.

### Timeout của HTTP server

| Timeout | Giá trị | Lý do |
|---|---|---|
| `ReadHeaderTimeout` | 5s | Chặn Slowloris |
| `ReadTimeout` | 30s | Giới hạn thời gian đọc request |
| `WriteTimeout` | `REQUEST_TIMEOUT + 5s` | Handler kịp trả lỗi timeout thay vì bị cắt kết nối giữa chừng |
| `IdleTimeout` | `HTTP_IDLE_TIMEOUT` | Phải **lớn hơn** idle timeout của LB (AWS ALB mặc định 60s). Nếu server đóng keep-alive connection trước LB, LB có thể gửi request vào đúng connection vừa đóng và trả 502 lác đác, rất khó tìm nguyên nhân |

### Readiness

Mặc định `/readyz` chỉ phản ánh trạng thái của chính pod (sẵn sàng hay đang shutdown), **không** ping database. Nếu readiness phụ thuộc database dùng chung, một sự cố database vài giây sẽ làm *mọi* pod cùng lúc unready: service mất hết endpoint và client nhận 503 từ LB, thay vì nhận lỗi nhanh rồi tự hồi phục. Theo dõi database bằng metrics và alert. Nếu vẫn muốn, bật `READINESS_CHECK_DB=true` (timeout 500ms) và đặt `failureThreshold` của probe đủ cao.

### Kích thước pool kết nối

App luôn đặt kích thước pool bằng `DB_MAX_CONNS`, không dùng mặc định của pgx (`max(4, runtime.NumCPU())`, mà trong container `NumCPU` là số core của *node*, không phải CPU limit của pod). Đặt `DB_MAX_CONNS` sao cho:

```
DB_MAX_CONNS × (số replica tối đa khi HPA scale + số pod tạm thời khi deploy) < max_connections của PostgreSQL
```

và chừa chỗ cho migration và kết nối quản trị. Khi số replica lớn, đặt PgBouncer phía trước.

### IP client

Sau LB, `RemoteAddr` là IP của LB. Đặt `TRUSTED_PROXIES` bằng đúng dải IP của LB/ingress; app chỉ tin các hop `X-Forwarded-For` nằm trong dải này. Các dải private mặc định *không* được tin, vì mọi pod trong cluster đều nằm trong mạng private và có thể giả header.

### Xác thực

Template **không có xác thực**, và `GET /api/v1/users` trả email của mọi user. Trước khi đưa lên môi trường thật, gắn middleware xác thực/phân quyền ở composition root, trong `internal/app/app.go`, nơi dựng `httpapi.Options`:

```go
e := httpapi.NewServer(users, httpapi.Options{
	// ... các option hiện có ...
	APIMiddleware: []echo.MiddlewareFunc{requireAuth}, // chỉ áp cho /api/v1, không áp cho probe
})

// Ví dụ tối giản; thay bằng cơ chế thật (JWT, OIDC, API key từ secret store...).
func requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if !validToken(c.Request().Header.Get("Authorization")) {
			return echo.ErrUnauthorized // thành 401 application/problem+json
		}
		return next(c)
	}
}
```

## Migration

Migration là file SQL trong `internal/adapter/outbound/postgres/migrations`, nhúng vào binary và chạy bằng [goose](https://github.com/pressly/goose):

```bash
DATABASE_URL='postgres://...?connect_timeout=10' userapi migrate
# Chạy local với database của docker compose: make migrate (bỏ qua DATABASE_URL; đổi DB bằng make migrate DB_URL=...)
```

- Bảng `goose_db_version` ghi lại version đã chạy; mỗi file chạy đúng một lần, trong transaction riêng.
- Advisory lock giữ trên một kết nối duy nhất, nên nhiều Job/replica chạy `migrate` cùng lúc vẫn an toàn.
- Phiên migration dùng kết nối riêng với `statement_timeout = 0` (tạo index có thể lâu) và `lock_timeout = 5s`: nếu `ALTER TABLE` phải chờ lock, nó thất bại sớm thay vì làm mọi query khác trên bảng xếp hàng phía sau.
- `userapi migrate` không tự đặt timeout kết nối: nếu host database nhận TCP nhưng không phản hồi, lệnh treo vô hạn. Thêm `connect_timeout` (giây) vào URL như trên, hoặc đặt `activeDeadlineSeconds` cho Job.
- Khi khởi động, app chỉ **đọc** trạng thái migration (không tạo bảng, không chạy DDL). Còn migration chưa chạy, kể cả database trống, thì app dừng ngay với thông báo `database has pending migrations`. Ngoại lệ: `MIGRATE_ON_START=true` (chỉ dành cho dev) cho app tự chạy migration trước khi kiểm tra.

**Trên Kubernetes:** chạy `userapi migrate` bằng một Job (Helm pre-upgrade hook, Argo CD PreSync) hoặc initContainer, dùng **cùng image** với app. Giữ `MIGRATE_ON_START=false`. `docker compose up` mô phỏng thứ tự này: service `migrate` chạy xong thì `api` mới khởi động. Nhưng cả hai dùng chung role `userapi` (superuser), nên compose **không** mô phỏng việc tách role ở mục dưới.

### Phân quyền database

Tách hai role: role migration sở hữu schema (DDL), role của app chỉ có DML. App bị lộ thông tin đăng nhập cũng không thể `DROP TABLE`. Chạy bằng superuser:

```sql
CREATE ROLE userapi_migrator LOGIN PASSWORD '...';
CREATE ROLE userapi_app      LOGIN PASSWORD '...';
CREATE DATABASE userapi OWNER userapi_migrator;
\connect userapi
-- PostgreSQL 15+ không còn cho mọi role quyền CREATE trên schema public.
GRANT USAGE, CREATE ON SCHEMA public TO userapi_migrator;
GRANT USAGE ON SCHEMA public TO userapi_app;
ALTER DEFAULT PRIVILEGES FOR ROLE userapi_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO userapi_app;
```

Job migration và app dùng **hai `DATABASE_URL` khác nhau**: Job dùng `userapi_migrator`, app dùng `userapi_app`. Default privileges ở trên chỉ áp cho bảng do `userapi_migrator` tạo, nên migration phải luôn chạy bằng role đó.

### Viết migration an toàn khi rolling deploy (expand/contract)

Trong lúc deploy, pod cũ và pod mới cùng chạy trên schema mới, nên mọi migration phải tương thích ngược với code đang chạy:

| Muốn | Làm |
|---|---|
| Thêm cột | Thêm cột nullable hoặc có `DEFAULT` (như `0002_add_users_version.sql`) |
| Đổi tên / xoá cột | Thêm cột mới → deploy code ghi cả hai → backfill → deploy code chỉ đọc cột mới → xoá cột cũ ở lần deploy sau |
| Tạo index trên bảng lớn | `CREATE INDEX CONCURRENTLY` trong file riêng có dòng `-- +goose NO TRANSACTION` (CONCURRENTLY không chạy được trong transaction) |
| Thêm `NOT NULL` / foreign key | `ADD CONSTRAINT ... NOT VALID`, sau đó `VALIDATE CONSTRAINT` ở migration riêng |

## Observability

**Log.** JSON ra stdout (trừ lỗi cấu hình lúc khởi động, xem [Cấu hình](#cấu-hình)). Mỗi request một dòng `http_request` (trừ `/healthz`, `/readyz`): 2xx/3xx ghi mức INFO, 4xx ghi WARN, 5xx ghi ERROR kèm lỗi gốc; `latency` tính bằng nano giây. `/readyz` trả 503 thì ghi một dòng WARN `readiness check failed` mỗi lần probe. Mọi dòng log ghi bằng `slog.*Context(ctx, ...)`, ở bất kỳ tầng nào, tự mang `request_id`; `trace_id` và `span_id` có khi request có span (OpenTelemetry bật, hoặc upstream gửi `traceparent`). Tìm log của một request bằng `X-Request-Id` trong response.

**Trace và metric (OpenTelemetry).** Bật bằng `OTEL_EXPORTER_OTLP_ENDPOINT` hoặc endpoint riêng cho trace/metric (OTLP/HTTP, xem bảng [Cấu hình](#cấu-hình)). Khi không đặt, provider là no-op. Lỗi export (collector không tới được) chỉ được ghi log mức INFO (`traces export: ...`), không làm app dừng; nếu lần flush cuối lúc tắt thất bại thì ghi WARN `telemetry shutdown failed`.

| Nguồn | Thu được |
|---|---|
| `otelhttp` (bọc ngoài Echo) | Span mỗi request, tên theo route (`GET /api/v1/users/:id`); metric `http.server.request.duration` có `http.route` |
| `otelpgx` | Span mỗi query (chỉ câu SQL, **không** kèm giá trị tham số, để không lộ PII) và span chờ lấy kết nối từ pool; metric của pool |

Probe không bị trace. Trace context từ upstream (`traceparent`) được tiếp nhận. Version của build (`-ldflags "-X main.version=..."`) được ghi vào resource `service.version` và dòng log khởi động.

## Xử lý sự cố

| Triệu chứng | Nguyên nhân thường gặp | Xử lý |
|---|---|---|
| App không khởi động: `database has pending migrations` | Job migration chưa chạy hoặc thất bại; hoặc database mới tinh | Chạy `userapi migrate` bằng role migrator; xem log của Job |
| `userapi migrate` lỗi `permission denied for schema public` / `failed to create version table` | Role migrator thiếu quyền `CREATE` trên schema `public` (PostgreSQL 15+) | `GRANT USAGE, CREATE ON SCHEMA public TO userapi_migrator` (xem [Phân quyền](#phân-quyền-database)) |
| `userapi migrate` lỗi `canceling statement due to lock timeout` | `ALTER TABLE` chờ lock quá 5s vì có transaction dài đang giữ bảng | Tìm transaction dài trong `pg_stat_activity`; chạy lại migration lúc ít tải |
| App không khởi động: `ping database` | Sai `DATABASE_URL`, mạng, hoặc database chưa sẵn sàng | Kiểm tra URL và kết nối; pod sẽ được Kubernetes khởi động lại |
| PostgreSQL báo `too many clients already` | Tổng `DB_MAX_CONNS` của mọi pod vượt `max_connections` | Giảm `DB_MAX_CONNS`, giới hạn số replica, hoặc thêm PgBouncer |
| 502 rải rác **mỗi lần deploy** | Listener đóng trước khi LB gỡ pod | Đặt `SHUTDOWN_DELAY` 5s–10s và `terminationGracePeriodSeconds` đủ lớn |
| 502 rải rác **bất kỳ lúc nào** | `HTTP_IDLE_TIMEOUT` nhỏ hơn idle timeout của LB | Đặt `HTTP_IDLE_TIMEOUT` lớn hơn của LB |
| Nhiều 409 kèm log `user update hit a version conflict, retrying` | Nhiều request cùng sửa một user | Kiểm tra client có retry dồn dập hay xử lý trùng lặp không |
| Nhiều 499 trong log | Client bỏ đi trước khi có response (thường do client timeout ngắn hơn latency) | Xem latency trên metric/trace; so với timeout phía client |
| Nhiều 503 `request timed out` | Request vượt `REQUEST_TIMEOUT` hoặc câu SQL vượt `DB_STATEMENT_TIMEOUT` (log có `SQLSTATE 57014`), thường do database chậm, lock, hoặc pool cạn kết nối | Xem span chờ lấy kết nối của `otelpgx`, metric pool, và lock trong `pg_stat_activity` |
| `remote_ip` trong log luôn là cùng một IP | Chưa đặt `TRUSTED_PROXIES` | Đặt bằng dải IP của LB/ingress |
| Mọi pod cùng unready khi database chập chờn | Đã bật `READINESS_CHECK_DB` | Tắt, hoặc tăng `failureThreshold` (xem [Readiness](#readiness)) |
