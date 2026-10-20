# Khởi tạo dự án từ template

Checklist cho team bắt đầu một service mới từ repo này. Làm theo thứ tự: bước 1–3 trong ngày đầu, bước 4–5 trước lần deploy production đầu tiên.

## 1. Tạo repo và đổi tên

Tạo repo mới từ template (trên GitHub: **Use this template**; chủ repo template phải bật *Template repository* trong Settings), clone về, rồi đổi module path và tên service. Ví dụ service `orders`:

```bash
NAME=orders
NEW=github.com/acme/$NAME

# Module path: .golangci.yml chứa đường dẫn package trong các rule depguard, nên phải đổi cùng.
go mod edit -module $NEW
grep -rl github.com/example/userapi --include='*.go' --include='.golangci.yml' . \
  | xargs sed -i "s#github.com/example/userapi#$NEW#g"

# Tên service: thư mục lệnh, binary, image, tên database/role của môi trường dev, application_name
# của PostgreSQL, service.name của OpenTelemetry, log, tài liệu, cấu hình Claude Code.
# Không sửa migration (comment trong đó không ảnh hưởng gì) và tài liệu lịch sử.
git mv cmd/userapi cmd/$NAME
git grep -l userapi -- ':!docs/superpowers' ':!go.sum' ':!internal/adapter/outbound/postgres/migrations' \
  | xargs sed -i "s/userapi/$NAME/g"

make check
```

Trên macOS, dùng `sed -i ''` thay cho `sed -i`. Quy trình trên đã được chạy thử: build, lint (depguard vẫn bắt import sai theo đường dẫn mới) và test đều xanh, không còn chỗ nào chứa `userapi` ngoài migration và `docs/superpowers/`.

Sau đó, sửa bằng tay:
- `LICENSE`: tên chủ sở hữu và năm.
- Tiêu đề và đoạn giới thiệu trong `README.md` và `README.vi.md`.
- `KAFKA_TOPIC` mặc định là `users`; đổi giá trị mặc định trong `internal/config/config.go` (và `.env.example`, `docs/operations.md`) nếu service phát sự kiện vào topic khác.
- Xoá `docs/superpowers/`: đó là lịch sử phát triển của template, không phải của dự án.

## 2. Bỏ phần không dùng

Mỗi phần dưới đây tắt được bằng cấu hình. Chỉ xoá code khi chắc chắn không cần, và xoá cả test, tài liệu, rule depguard liên quan trong cùng commit.

| Phần | Tắt bằng | Xoá hẳn |
|---|---|---|
| Phát sự kiện (outbox, relay, Kafka) | `EVENTS_SINK=none` (mặc định) | `internal/outbox`, `internal/adapter/outbound/kafka`, `internal/app/relay.go`, `postgres/outbox*.go`, `WithOutbox`, nhánh `relay` trong `cmd`, `docs/events.md`, rule `outbox`/`outbound-adapters-no-broker` và mục franz-go trong `.golangci.yml`, `.claude/rules/messaging.md`. Migration 0003, 0004 giữ lại nếu database đã chạy chúng; với database mới thì xoá được |
| Repository in-memory | Đặt `STORAGE=postgres` ở mọi môi trường | Giữ lại: test của service và HTTP dùng nó |

## 3. Thiết lập GitHub

- **Branch protection** cho `main`: bắt buộc PR, bắt buộc các check `lint`, `test`, `vulncheck`, `docker` xanh, cấm force-push.
- Bật **Dependabot alerts** và **Dependabot security updates** (Settings › Code security). `.github/dependabot.yml` đã có sẵn cho Go modules, GitHub Actions và image Docker, chỉ đề xuất bản phát hành ít nhất 14 ngày.
- Bật **secret scanning** và **push protection**.
- CI chạy trên mọi PR, khi push lên `main`, và hằng tuần (để govulncheck bắt lỗ hổng mới công bố).

## 4. Quyết định trước khi lên production

Template cố ý không chọn thay những thứ này. Ghi quyết định vào `docs/` của dự án.

| Việc | Template hiện tại | Cần quyết định |
|---|---|---|
| Xác thực, phân quyền | Không có. `GET /api/v1/users` trả email của mọi user | Cơ chế (JWT, OIDC, mTLS...) và nơi gắn: xem [operations.md › Xác thực](operations.md#xác-thực) |
| TLS cho HTTP | Server chỉ nói HTTP; giả định TLS kết thúc ở ingress hoặc load balancer | Xác nhận với hạ tầng; đặt `TRUSTED_PROXIES` bằng dải IP của LB |
| TLS tới PostgreSQL | Mọi ví dụ dùng `sslmode=disable` | Production dùng `sslmode=verify-full` (kèm `sslrootcert` nếu CA nội bộ) trong `DATABASE_URL` |
| CORS | Không có header CORS: trình duyệt ở domain khác không gọi được API | Chỉ thêm nếu có frontend gọi trực tiếp; middleware của Echo, gắn ở `internal/app` |
| Rate limiting | Không có | Ở API gateway/ingress, hoặc middleware nếu cần theo từng user |
| Security header | Không có (`Strict-Transport-Security`, `X-Content-Type-Options`...) | Thường đặt ở ingress |
| Secret | Đọc từ biến môi trường | Đưa `DATABASE_URL`, `KAFKA_SASL_PASSWORD` vào Secret manager / Kubernetes Secret, không vào image hay repo |
| Image và release | CI build image nhưng không push; base image theo tag | Registry, quy ước tag (`git describe`), job push khi tạo tag; cân nhắc pin digest của base image (Dependabot cập nhật được) |
| Kafka (nếu bật sự kiện) | TLS và SASL (PLAIN, SCRAM) có sẵn qua `KAFKA_TLS`, `KAFKA_SASL_*` | Tạo topic, ACL cho user của relay; Amazon MSK dùng SASL/SCRAM qua TLS (IAM chưa hỗ trợ). Xem [operations.md › Phát sự kiện](operations.md#phát-sự-kiện) |

## 5. Kiểm tra cuối

- `make check` xanh với `TEST_DATABASE_URL` (test PostgreSQL chạy, không bị skip).
- CI xanh trên PR đầu tiên.
- Deploy thử theo [operations.md](operations.md): Job `migrate` với role migrator, API với role app, probe `/healthz` và `/readyz`, `SHUTDOWN_DELAY`, `terminationGracePeriodSeconds`.
- Nếu bật sự kiện: Deployment `relay` với liveness probe `GET /healthz` ở `RELAY_HEALTH_ADDR`, và hai cảnh báo trong operations.md (outbox tồn đọng, bản ghi bị tách riêng).
