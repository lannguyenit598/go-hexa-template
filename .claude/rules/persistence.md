---
paths:
  - "internal/adapter/outbound/**/*.go"
  - "internal/adapter/outbound/postgres/migrations/*.sql"
---

# Adapter outbound và persistence

- Mỗi adapter outbound độc lập: không import adapter outbound khác, adapter inbound, Echo hay `internal/app` (depguard chặn). Thêm adapter mới thì thêm đường dẫn của nó vào rule `outbound-adapters` trong `.golangci.yml`.
- Repository phải đúng hợp đồng trong comment của `port.UserRepository` và **vượt qua `repotest.Run`**. Hành vi mới của repository được thêm vào `repotest` trước, để mọi adapter cùng bị kiểm tra.
- Trả sentinel trần (`port.ErrNotFound`, `port.ErrVersionConflict`, `domain.ErrEmailAlreadyExists`); dịch lỗi driver trong `mapError`, không để `pgx`/`pgconn` lọt ra ngoài adapter. Lỗi timeout phía database (`57014`, `statement_timeout`) được bọc thành `context.DeadlineExceeded` để HTTP trả 503.
- `Update` là compare-and-set trên `version` trong **một** câu `UPDATE ... WHERE id = $1 AND version = $n`; không dùng `SELECT ... FOR UPDATE`. Khi bật outbox, câu đó và câu ghi outbox nằm trong một transaction ngắn (`r.write`).
- Lệnh ghi mới của repository đi qua `r.write` và gọi `r.appendEvent` khi thành công, để sự kiện luôn commit cùng dữ liệu (xem `.claude/rules/messaging.md`).
- SQL chỉ dùng tham số (`$1`), không nối chuỗi.
- Trace không được chứa giá trị tham số (PII): giữ `otelpgx` ở cấu hình mặc định, không bật `WithIncludeQueryParameters`.
- Migration: **không bao giờ sửa file đã merge**; luôn thêm file mới. Dùng skill `/add-migration`.
- Kiểm tra trạng thái migration ở runtime phải chỉ đọc. `goose.HasPending` tự tạo bảng `goose_db_version`, nên luôn kiểm tra trước bằng `to_regclass`, như `PendingMigrations`.
