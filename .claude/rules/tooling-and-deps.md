---
paths:
  - "go.mod"
  - "go.sum"
  - ".golangci.yml"
  - ".github/**"
  - "Makefile"
---

# Dependency, lint và CI

- Sau mọi thay đổi `go.mod`: chạy `go mod tidy`; CI kiểm tra bằng `go mod tidy -diff`.
- Trước khi nâng một module, đọc dòng `go` trong `go.mod` của phiên bản đích. Nếu nó cao hơn dòng `go` của repo, việc nâng sẽ kéo cả repo lên; khi đó cập nhật cùng lúc:
  - README (cả hai bản) và CI;
  - phiên bản golangci-lint trong `.github/workflows/ci.yml` và trong `CLAUDE.md`: golangci-lint phải được build bằng Go ≥ dòng `go` của repo.
- govulncheck đỏ vì một module: nâng module đó lên phiên bản "Fixed in" mà báo cáo nêu, rồi xử lý hệ quả theo mục trên.
- `.golangci.yml`:
  - không tắt linter, không thêm exclusion để vượt qua một lỗi cụ thể; sửa code;
  - đổi module path thì phải đổi cả đường dẫn package trong các rule depguard (`grep -c github.com/example/userapi .golangci.yml` để đếm; bỏ sót thì rule âm thầm không còn tác dụng);
  - sau khi sửa rule depguard, thêm thử một file import vi phạm, xác nhận lint bắt được, rồi xoá file đó. File thử phải import một package **có thật** (package không tồn tại làm typecheck lỗi trước, depguard không chạy);
  - `forbidigo` và `gochecknoglobals` chỉ áp dụng cho `internal/core` (exclusion `path-except`); đó là phạm vi của quy tắc, không phải ngoại lệ để vượt lint.
- CI chạy trên PR vào `main` và khi push lên `main`; không có job nào được phép đỏ khi merge.
