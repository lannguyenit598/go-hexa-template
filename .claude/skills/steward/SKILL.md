---
name: steward
description: Use when driving a pull request of this userapi repo to green or mergeable — a CI job (lint, test, vulncheck, docker) failed, a review comment arrived, the branch has a merge conflict, or a PR is about to be opened.
---

# Đưa PR tới trạng thái xanh

## Trước khi mở PR

- Base là `main`, compare là nhánh làm việc. (Một PR mở ngược chiều sẽ merge `main` vào nhánh và không thay đổi gì trên `main`.)
- Nếu PR trước của nhánh đã merge: tạo lại nhánh từ `origin/main` mới nhất trước khi làm tiếp. Không chồng commit mới lên lịch sử đã merge.
- `make check` xanh trên máy, kèm `TEST_DATABASE_URL` nếu có PostgreSQL. Ghi rõ trong mô tả PR phần nào chưa kiểm chứng được trên máy (ví dụ govulncheck khi không truy cập được `vuln.go.dev`).

## Khi một job CI đỏ

Đọc log của đúng job đó trước. Tái hiện trên máy, sửa, chạy lại `make check`, rồi mới push.

| Job | Tái hiện trên máy | Nguyên nhân hay gặp và cách sửa |
|---|---|---|
| `lint` | `golangci-lint run ./...` (đúng phiên bản trong `ci.yml`) và `go mod tidy -diff` | **Lint từ chối chạy vì phiên bản Go**: golangci-lint phải được build bằng Go ≥ dòng `go` trong `go.mod`, nên nâng phiên bản trong `ci.yml` và `CLAUDE.md`. **`noctx` báo lỗi mới sau khi nâng lint**: dùng `httptest.NewRequestWithContext`, `net.ListenConfig{}.Listen(ctx, …)`. **depguard báo lỗi**: import sai hướng, sửa code, không sửa `.golangci.yml` |
| `test` | `TEST_DATABASE_URL=... go test -race -count=1 ./...`, rồi `DATABASE_URL=$TEST_DATABASE_URL go run ./cmd/userapi migrate` hai lần (CI kiểm tra migration chạy lặp lại được) | **Test lifecycle timeout khi chạy cùng package khác**: advisory lock của goose (`4097083626`) dùng chung cả database và thử lại mỗi 5s, nên deadline của test phải ≥ 15s. Tái hiện bằng cách giữ lock: `SELECT pg_advisory_lock(4097083626); SELECT pg_sleep(8);` trong psql, rồi chạy test. **Dữ liệu lẫn giữa các test**: package `postgres` TRUNCATE bảng chung; test cần database sạch dùng `pgtest.FreshSchemaDSN` |
| `vulncheck` | `go run golang.org/x/vuln/cmd/govulncheck@<phiên bản trong ci.yml> ./...` (cần truy cập `vuln.go.dev`; nếu bị chặn thì đọc log CI) | Nâng module bị báo lên phiên bản "Fixed in" (`go get <module>@<version>`, `go mod tidy`). Kiểm tra dòng `go` trong `go.mod` của phiên bản mới: nếu cao hơn của repo, cập nhật đồng thời README (cả hai bản), CI và phiên bản golangci-lint |
| `docker` | `docker build .` | Image build trong `Dockerfile` (`golang:X-alpine`) phải có Go ≥ dòng `go` trong `go.mod` |

**Flaky không phải là nguyên nhân.** Chỉ chạy lại job một lần khi lỗi xảy ra trước khi test chạy (checkout, cài đặt, runner mất). Lỗi lặp lại là lỗi thật: tìm nguyên nhân. Không skip hay tắt test để CI xanh.

## Merge conflict

- Merge `main` vào nhánh (không rebase hay force-push nhánh người khác đang dùng).
- `go.sum` bị conflict: lấy bản của một bên rồi chạy `go mod tidy`; không sửa tay.
- Conflict ở `README.md`/`README.vi.md`: giải quyết cả hai bản theo cùng một nội dung.

## Review comment

- Yêu cầu nhỏ, cục bộ (đổi tên, thêm test, sửa một hàm): làm, push, trả lời ngắn trên thread.
- Yêu cầu lớn (đổi hợp đồng API, đổi port, refactor nhiều file): đề xuất cách làm trong thread, chờ chủ PR quyết định.
- Comment cho rằng có bug: tự dựng một test tái hiện trước khi sửa. Không tái hiện được thì trả lời kèm bằng chứng.
