---
paths:
  - "internal/core/**/*.go"
---

# Core (domain, port, service)

- Chỉ import thư viện chuẩn và `internal/core/...`. Cần thứ gì từ bên ngoài (thời gian, ID, lưu trữ, gửi email…) thì khai báo một interface trong `port` và để adapter hiện thực.
- `domain`:
  - Invariant được giữ bằng constructor và method (`NewUser`, `Apply`, `ParseEmail`). Giá trị có quy tắc riêng thì làm value object như `Email`.
  - Hàm dựng lại từ storage (`EmailFromStore`) **không** validate lại, để thắt chặt quy tắc sau này không làm hỏng việc đọc dữ liệu cũ.
  - `Apply` là hàm thuần: trả bản sao mới, chỉ tăng `Version`/`UpdatedAt` khi dữ liệu thực sự đổi.
  - Không mang tag JSON/DB.
- `port`:
  - Inbound command/query chỉ chứa primitive; outbound interface dùng kiểu domain.
  - Lỗi kết quả use case dùng chung (`ErrNotFound`, `ErrVersionConflict`) đặt ở đây, không tạo `ErrXxxNotFound` riêng cho từng aggregate.
  - Mọi thay đổi hợp đồng của `UserRepository` phải đi kèm test trong `internal/adapter/outbound/repotest`.
- `service`:
  - Validate/parse input (ví dụ `parseUserID`) **trước** khi gọi repository.
  - Wrap lỗi kèm ngữ cảnh bằng `%w`.
  - Log qua `s.logger` (được inject), không dùng `slog.Default()`/`slog.Info(...)`.
  - Retry chỉ khi thao tác lặp lại an toàn (như `Update`: đọc lại rồi `Apply` lại).
- Test service dùng `fakeClock`/`seqIDs` và repository in-memory; mô phỏng tranh chấp bằng repo bọc ngoài (xem `racingRepo`).
