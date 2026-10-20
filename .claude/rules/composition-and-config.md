---
paths:
  - "cmd/**/*.go"
  - "internal/app/**/*.go"
  - "internal/config/**/*.go"
  - "internal/observability/**/*.go"
  - ".env.example"
  - "docker-compose.yml"
  - "Dockerfile"
---

# Composition root, cấu hình, hạ tầng dùng chung

- `cmd/userapi/main.go` chỉ: đọc config, tạo logger, bắt tín hiệu, chọn lệnh, gọi `app.Run`/`app.Migrate`. Logic vòng đời đặt trong `internal/app`, nơi có test.
- `internal/app` là nơi duy nhất lắp adapter vào core (`newRepository`, `httpapi.NewServer`). Mọi thứ mở ra trong `Run` phải được đóng trên **mọi** nhánh lỗi lúc khởi động.
- Thứ tự tắt phải giữ nguyên: đánh dấu draining → chờ `ShutdownDelay` → `srv.Shutdown` → đóng pool → flush telemetry. Khi `srv.Shutdown` quá hạn, huỷ context gốc của request (`BaseContext`) và `srv.Close()` **trước** khi đóng pool; nếu không, `pool.Close()` chờ query chạy xong và tiến trình sống quá ngân sách. Có test trong `internal/app/app_test.go`; chạy lại khi sửa.
- `newRepository` dùng `switch` có nhánh `default` trả lỗi: mỗi giá trị `Storage` phải có nhánh riêng.
- Thêm biến môi trường thì làm đủ trong cùng commit:
  1. đọc và validate trong `internal/config/config.go`, lỗi gom qua `parser`;
  2. thêm case vào `config_test.go` (giá trị mặc định và giá trị sai);
  3. thêm vào `.env.example`;
  4. thêm vào bảng cấu hình trong `docs/operations.md` (mặc định, ý nghĩa, ràng buộc).
- `internal/config` và `internal/observability` không import core, adapter hay `internal/app` (rule depguard `platform`).
- Lời gọi I/O mạng phải nhận context: `net.ListenConfig{}.Listen(ctx, …)`, `http.NewRequestWithContext` (lint `noctx`).
