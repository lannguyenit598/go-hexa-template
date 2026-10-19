---
paths:
  - "**/*_test.go"
  - "internal/adapter/outbound/repotest/**"
  - "internal/adapter/outbound/postgres/pgtest/**"
---

# Viết test

- TDD: chạy test và thấy nó **fail đúng lý do** trước khi sửa code. Test pass ngay từ đầu nghĩa là nó chưa kiểm tra điều bạn nghĩ.
- Đặt tên test theo hành vi (`TestUpdate_EmptyPatchIsNoOp`); dùng table test khi nhiều input cùng một kỳ vọng.
- Không gọi `t.Fatal`/`t.FailNow` trong goroutine khác goroutine của test: dựng mọi thứ có thể fail (config, listener…) trước khi `go func()`, và trả lỗi về qua channel.
- Không dùng `time.Sleep` để chờ một điều kiện; polling có deadline (xem `startApp` trong `internal/app/app_test.go`). Deadline phải rộng hơn mọi cơ chế chờ bên dưới; ví dụ advisory lock của goose thử lại mỗi 5s, nên test có migrate cần chờ ≥ 15s.
- HTTP client trong test: tắt keep-alive (`DisableKeepAlives: true`) khi kiểm tra việc server đóng kết nối; tạo request bằng `NewRequestWithContext`.
- Test cần PostgreSQL:
  - đọc `TEST_DATABASE_URL` và tự skip khi thiếu;
  - cần schema sạch, riêng biệt (database trống, migration) thì dùng `pgtest.FreshSchemaDSN(t)`;
  - test trong package `postgres` dùng chung bảng `users` và `TRUNCATE` nó, nên không giả định dữ liệu do test khác tạo ra.
- `go test ./...` chạy các package **song song** trên cùng database: không dựa vào thứ tự giữa các package.
