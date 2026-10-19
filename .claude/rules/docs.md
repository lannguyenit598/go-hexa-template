---
paths:
  - "**/*.md"
  - "api.http"
---

# Tài liệu

- README có hai bản và phải đồng bộ: `README.md` (tiếng Anh, mặc định trên GitHub) và `README.vi.md` (tiếng Việt). Sửa một bản thì sửa bản kia trong cùng commit.
- `docs/api.md`, `docs/operations.md`, `docs/architecture.md` viết bằng tiếng Việt, mỗi file cho một nhóm người đọc; đặt nội dung vào đúng file, link thay vì chép lại.
- Mọi khẳng định về hành vi (status code, nguyên văn `detail`, header, giá trị mặc định, lệnh, câu SQL) phải được kiểm chứng bằng cách chạy thật trước khi ghi. Lệnh hướng dẫn người dùng chạy (script, SQL) phải được chạy thử nguyên văn.
- Sau khi sửa, kiểm tra mọi link tương đối và anchor `#...` còn trỏ đúng.
- Không sửa `docs/superpowers/`: đó là bản ghi lịch sử.
