# Spec: Tinh chỉnh kiến trúc Hexagonal (mục 1–5 của bản review 2026-10-19)

Nguồn: bản review kiến trúc Hexagonal sau 7 commit trên branch `claude/vibrant-keller-k2m8al`.
Phạm vi: chỉ mục 1–5. ADR-001 (tổ chức package theo bounded context) **không** thuộc phạm vi;
chỉ làm khi xuất hiện aggregate thứ hai.

## R1. Composition root mỏng, lifecycle có test
- `cmd/userapi/main.go` chỉ còn: đọc config, tạo logger, bắt tín hiệu, chọn lệnh (`""` hoặc `migrate`).
- Toàn bộ khởi tạo adapter, readiness, drain khi shutdown, timeout của `http.Server`, flush telemetry,
  kiểm tra migration tồn đọng chuyển sang package `internal/app`, có test tự động cho:
  - trong `SHUTDOWN_DELAY` sau khi ctx bị huỷ: `/readyz` = 503, API vẫn = 200;
  - sau shutdown: listener đã đóng, `Run` trả `nil`;
  - không khởi động được (cổng bị chiếm) thì `Run` trả lỗi, không treo;
  - database còn migration chưa chạy (kể cả database trống hoàn toàn) thì `Run` trả lỗi chứa
    `pending migrations`; với `MIGRATE_ON_START=true` thì chạy bình thường.
- Hành vi giữ nguyên: tín hiệu thứ hai kết thúc tiến trình ngay.

## R2. Inbound port chỉ dùng kiểu primitive
- Command/query đầu vào của `port.UserService` chỉ chứa kiểu primitive (`string`, `*string`, `int64`...);
  không chứa `domain.UserID`, `domain.Changes`.
- Service chịu trách nhiệm chuyển sang kiểu domain và validate ID (ID sai định dạng thì trả not found).
- Kết quả trả về vẫn là `domain.User` (adapter được phép đọc kiểu domain).
- Hợp đồng HTTP (status, JSON) không đổi.

## R3. Service không dùng logger toàn cục
- `service.UserService` nhận `*slog.Logger` qua constructor; `nil` = không log.
- Dòng log khi retry vì xung đột version đi qua logger được inject, vẫn dùng `*Context(ctx, ...)`.

## R4. depguard bịt các chỗ hở
- `internal/observability`, `internal/config` không được import adapter, core, app.
- Adapter outbound không import adapter outbound khác, không import bộ test `repotest`.
- Không package nào ngoài `cmd` import `internal/app`.

## R5. Lỗi kết quả use case nằm ở tầng application
- `ErrNotFound`, `ErrVersionConflict` chuyển từ `domain` sang `port` và dùng chung cho mọi aggregate
  (không có `ErrUserNotFound`). Ngữ cảnh được thêm bằng wrap (`fmt.Errorf("user %s: %w", ...)`).
- `domain` chỉ giữ lỗi của nghiệp vụ: `ValidationError`, `ErrEmailAlreadyExists`.
- Ánh xạ HTTP không đổi: not found = 404, version conflict = 409 (412 khi có `If-Match`).
