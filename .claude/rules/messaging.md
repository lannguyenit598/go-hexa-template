---
paths:
  - "internal/outbox/**/*.go"
  - "internal/adapter/outbound/kafka/**/*.go"
  - "internal/adapter/outbound/postgres/outbox*.go"
  - "internal/app/relay.go"
  - "docs/events.md"
---

# Sự kiện và message broker

- Sự kiện chỉ đi qua outbox: ghi trong cùng transaction với thay đổi dữ liệu (`r.write` + `r.appendEvent` trong adapter postgres). Không bao giờ publish thẳng lên broker từ request HTTP (dual write: commit xong mà publish lỗi là mất sự kiện).
- Core không biết sự kiện tồn tại: `git diff main -- internal/core` phải rỗng trong mọi thay đổi chỉ liên quan tới sự kiện. Sự kiện cần thông tin chỉ service biết (người thực hiện, lý do, giá trị cũ) thì đọc mục "Phát sự kiện qua transactional outbox" trong `docs/architecture.md` trước.
- Sự kiện mỏng: không đưa email, tên hay dữ liệu cá nhân khác vào payload, header, log hay trace.
- Giao ít nhất một lần: không hứa "đúng một lần" trong tài liệu hay code; consumer bỏ trùng theo `event_id` hoặc `version`.
- Schema (`docs/events.md`): trong cùng `schema_version` chỉ được thêm field. Đổi tên, đổi kiểu, xoá field thì tăng `schema_version`. Đổi schema hay thêm loại sự kiện là đổi hợp đồng: cập nhật `docs/events.md` cùng commit.
- `internal/outbox` chỉ biết `Source`/`Sink`, không import adapter, `pgx` hay `franz-go` (rule depguard `everything-else`). Broker mới = adapter mới hiện thực `outbox.Sink` + nhánh trong `newSink` (`internal/app/relay.go`).
- Test Kafka dùng `kfake` (cluster in-memory), không cần broker thật. Test outbox trên PostgreSQL dùng `pgtest.FreshSchemaDSN` và migrate schema riêng.
