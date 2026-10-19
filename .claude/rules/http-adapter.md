---
paths:
  - "internal/adapter/inbound/**/*.go"
---

# Adapter HTTP (`internal/adapter/inbound/httpapi`)

- Handler mỏng: bind DTO → gọi `port.UserService` → map kết quả sang DTO. Không chứa logic nghiệp vụ, không import adapter outbound hay `internal/app`.
- Truyền thẳng giá trị thô (ví dụ `c.Param("id")`) vào inbound port; không tự dựng `domain.UserID`, `domain.Changes`.
- DTO JSON (`userResponse`, `fieldError`…) nằm ở adapter, không thêm tag JSON vào kiểu domain.
- Lỗi: handler chỉ `return err`; ánh xạ sang status nằm trong `toProblem` (`errors.go`). Ngoại lệ duy nhất hiện có: `update` trả 412 khi có `If-Match`. Mọi response lỗi phải là `application/problem+json`; lỗi 5xx không lộ chi tiết nội bộ.
- Đổi status code, header, body hay thông báo `detail` là đổi hợp đồng API: cập nhật `docs/api.md` (bảng lỗi có nguyên văn `detail`) và `api.http` trong cùng commit.
- Route mới dưới `/api/v1` tự đi qua `APIMiddleware`; probe (`/healthz`, `/readyz`) không bị log/trace và không đi qua middleware xác thực.
- Test bằng `httptest.NewRequestWithContext` (lint `noctx` cấm `httptest.NewRequest`), dùng helper `newAPI`/`newAPIWith`/`expectStatus` sẵn có, và kiểm tra cả `Content-Type: application/problem+json` cho response lỗi.
