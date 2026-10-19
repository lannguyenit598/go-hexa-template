---
name: add-migration
description: Use when changing the PostgreSQL schema of this userapi repo — adding/renaming/dropping a column, table, index or constraint, or when a feature needs new persisted data.
argument-hint: "[thay đổi schema]"
---

# Thêm migration

Thay đổi schema cần làm: $ARGUMENTS

Pod cũ và pod mới cùng chạy trên schema mới trong lúc rolling deploy, nên mọi migration phải **tương thích ngược với code đang chạy** (expand/contract). Migration chạy bằng role migrator, với `lock_timeout = 5s` và không giới hạn `statement_timeout`.

## 1. Chọn chiến lược

| Muốn | Làm | Số lần deploy |
|---|---|---|
| Thêm cột | Cột nullable hoặc có `DEFAULT` hằng số | 1 |
| Thêm `NOT NULL` cho cột đã có | Backfill trước; `ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID`, rồi `VALIDATE CONSTRAINT` ở migration sau | 2 |
| Đổi tên / đổi kiểu / xoá cột | Thêm cột mới → code ghi cả hai → backfill → code chỉ đọc cột mới → xoá cột cũ | 3+ |
| Index trên bảng có dữ liệu | `CREATE INDEX CONCURRENTLY` trong **file riêng** có `-- +goose NO TRANSACTION` | 1 |
| Foreign key | `ADD CONSTRAINT ... NOT VALID`, rồi `VALIDATE CONSTRAINT` ở migration riêng | 2 |

Nếu thay đổi cần nhiều lần deploy, nói rõ với người dùng và chỉ làm bước đầu trong PR này.

## 2. Viết file

- **Không sửa file migration đã merge**; luôn tạo file mới.
- Đường dẫn: `internal/adapter/outbound/postgres/migrations/NNNN_mo_ta_ngan.sql`, với `NNNN` là số kế tiếp, 4 chữ số (xem `ls` thư mục).
- Định dạng goose:

```sql
-- +goose Up
-- Lý do và chiến lược (expand/contract), viết bằng tiếng Việt.
ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS phone;
```

- File có `CREATE INDEX CONCURRENTLY` phải bắt đầu bằng `-- +goose NO TRANSACTION`, và chỉ chứa câu lệnh đó.

## 3. Cập nhật code persistence

Trong `internal/adapter/outbound/postgres/user_repository.go`: struct `userRow`, hằng `selectCols`, câu `INSERT`/`UPDATE`, `toDomain`. Code mới phải chạy được cả khi cột mới còn rỗng (dữ liệu cũ).

## 4. Kiểm chứng (cần PostgreSQL, đặt `TEST_DATABASE_URL`)

1. Migration chạy được trên database trống: `TestPendingMigrations_EmptySchema` áp dụng toàn bộ migration trên một schema mới.
2. Chạy hai lần liên tiếp vẫn thành công (lần hai không làm gì): `DATABASE_URL=$TEST_DATABASE_URL go run ./cmd/userapi migrate`.
3. Contract test và toàn bộ suite: `make check`.
4. Nếu có `Down`: chạy thử up → down → up trên một schema tạm (lần đầu mất khoảng 1 phút để tải goose CLI):
   ```bash
   S=down_check
   psql "$TEST_DATABASE_URL" -c "DROP SCHEMA IF EXISTS $S CASCADE; CREATE SCHEMA $S"
   G="go run github.com/pressly/goose/v3/cmd/goose@v3.27.0 -dir internal/adapter/outbound/postgres/migrations postgres"
   DSN="$TEST_DATABASE_URL&search_path=$S"   # TEST_DATABASE_URL đã có '?', nên nối bằng '&'
   $G "$DSN" up && $G "$DSN" down && $G "$DSN" up
   psql "$TEST_DATABASE_URL" -c "DROP SCHEMA $S CASCADE"
   ```
   Giữ phiên bản goose CLI trùng với `go.mod`.

## 5. Tài liệu

Có ví dụ hay quy tắc mới cho việc viết migration thì cập nhật mục expand/contract trong `docs/operations.md`.

## Lỗi thường gặp

| Lỗi | Hậu quả | Đúng là |
|---|---|---|
| Sửa file migration đã merge | Database đã chạy file cũ không bao giờ nhận thay đổi | File mới |
| `ADD COLUMN ... NOT NULL` không có `DEFAULT` | Lỗi trên bảng có dữ liệu; pod cũ không `INSERT` được | Nullable/`DEFAULT`, `NOT NULL` ở bước sau |
| `CREATE INDEX CONCURRENTLY` chung file với lệnh khác | Lỗi "cannot run inside a transaction block" | File riêng + `NO TRANSACTION` |
| `CREATE INDEX` thường trên bảng lớn | Khoá ghi toàn bảng trong lúc tạo index | `CONCURRENTLY` |
| Xoá/đổi tên cột trong cùng lần deploy với code mới | Pod cũ lỗi `column does not exist` | Expand/contract qua nhiều lần deploy |
