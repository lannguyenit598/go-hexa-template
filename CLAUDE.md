# userapi: hướng dẫn cho Claude

REST API quản lý user viết bằng Go 1.26 + Echo v5, theo kiến trúc Hexagonal. Tài liệu đầy đủ: `docs/architecture.md` (cho người phát triển), `docs/api.md` (hợp đồng HTTP), `docs/operations.md` (cấu hình, vận hành). Quy tắc riêng cho từng khu vực code nằm trong `.claude/rules/` và tự nạp khi bạn mở file tương ứng.

## Lệnh

| Việc | Lệnh |
|---|---|
| Kiểm tra trước khi commit (bắt buộc) | `make check`: gofmt, `go mod tidy -diff`, golangci-lint (gồm depguard), `go test -race -count=1 ./...` |
| Test kèm PostgreSQL | Đặt `TEST_DATABASE_URL=postgres://...` rồi `make check`. Không có biến này, test PostgreSQL bị skip: nói rõ điều đó khi báo kết quả |
| Chạy API | `make run` (in-memory) hoặc `make run-pg` (cần Docker) |
| Migration | `DATABASE_URL=... go run ./cmd/userapi migrate` |

Công cụ: Go ≥ 1.26 và golangci-lint **v2.14.0** (phải là bản build bằng Go ≥ phiên bản trong `go.mod`, nếu không lint sẽ từ chối chạy). CI ở `.github/workflows/ci.yml` chạy đúng các bước của `make check` cộng thêm govulncheck và build Docker.

## Kiến trúc: quy tắc không được phá

- Import chỉ hướng vào trong: `domain` ← `port` ← `service` ← adapter ← `internal/app` ← `cmd`. `internal/core/**` chỉ import thư viện chuẩn và chính `internal/core`. depguard trong `.golangci.yml` kiểm tra quy tắc này; **không** sửa `.golangci.yml` để lint xanh, hãy sửa code.
- Inbound port (`port.UserService`) chỉ nhận kiểu primitive (`string`, `*string`, `int64`); service tự chuyển sang kiểu domain và validate. Kết quả trả về được dùng kiểu domain.
- Lỗi: quy tắc nghiệp vụ ở `domain` (`ValidationError`, `ErrEmailAlreadyExists`); kết quả use case ở `port` (`ErrNotFound`, `ErrVersionConflict`, dùng chung mọi aggregate). Repository trả sentinel trần; service wrap kèm ngữ cảnh (`fmt.Errorf("user %s: %w", id, port.ErrNotFound)`); so sánh bằng `errors.Is`/`errors.As`. Core không biết HTTP status.
- Core không dùng state toàn cục: logger, đồng hồ, bộ sinh ID đều được truyền vào qua constructor/port (lint `forbidigo` và `gochecknoglobals` kiểm tra trong `internal/core`).
- Chỉ composition root được biết adapter: `internal/app` lắp ráp, `cmd/userapi` chỉ gọi `internal/app`. Package mới nằm ngoài các nhóm đã có rule thì rơi vào rule depguard `everything-else` (không được import adapter).

## Quy ước

- Comment trong code và tài liệu trong `docs/`: tiếng Việt. Commit message: tiếng Anh, dòng đầu ở thể mệnh lệnh, thân giải thích *vì sao*.
- Không thêm abstraction chưa có use case dùng (ví dụ Unit of Work: chỉ thêm khi thật sự cần, theo mẫu trong `docs/architecture.md`).
- Dependency mới: ưu tiên phiên bản đã phát hành ít nhất 2 tuần; trước khi nâng, kiểm tra dòng `go` trong `go.mod` của module đó (`go mod download -json <module>@<version>` rồi đọc file `GoMod`). Một bản nâng cấp có thể kéo mức Go tối thiểu của cả repo lên. Bản vá bảo mật được phép mới hơn 2 tuần.
- Nhánh và PR: PR luôn có **base `main`**, compare là nhánh làm việc. Không push thẳng lên `main`.

## Hoàn thành nghĩa là

1. `make check` xanh; nêu rõ test PostgreSQL đã chạy hay bị skip.
2. Hành vi mới có test đã được thấy **fail trước** khi sửa (TDD).
3. Tài liệu được cập nhật cùng commit khi đổi:
   - hợp đồng HTTP → `docs/api.md` và `api.http`;
   - biến môi trường → `docs/operations.md`, `.env.example`, `internal/config/config_test.go`;
   - kiến trúc hoặc quy tắc phụ thuộc → `docs/architecture.md` và `.golangci.yml`;
   - nội dung README → sửa **cả** `README.md` (tiếng Anh) và `README.vi.md` (tiếng Việt).
4. Mọi khẳng định trong tài liệu về hành vi (status code, thông báo lỗi, lệnh) đã được kiểm chứng bằng cách chạy thật, không suy ra từ việc đọc code.

## Quy trình có sẵn

| Khi | Dùng skill |
|---|---|
| Thêm hoặc đổi một use case / endpoint | `/add-use-case` |
| Thêm migration hoặc đổi schema | `/add-migration` |
| Theo dõi, sửa CI, trả lời review trên PR | `steward` (tự nạp khi làm việc với PR) |

`docs/superpowers/` là tài liệu lịch sử (spec và plan cũ), không dùng làm nguồn sự thật.
