---
name: add-use-case
description: Use when adding or changing a use case, endpoint, or repository method in this Go hexagonal userapi repo — e.g. "add search by email", "add a field to users", "new endpoint under /api/v1", or changing what an existing endpoint accepts or returns.
argument-hint: "[mô tả use case]"
---

# Thêm hoặc đổi một use case

Đi từ trong ra ngoài (domain → port → adapter → HTTP → docs), mỗi tầng theo TDD: viết test, **chạy và thấy nó fail đúng lý do**, rồi mới sửa code. Use case cần làm: $ARGUMENTS

## 0. Chốt hợp đồng trước khi code

Viết ra (trong câu trả lời hoặc PR), rồi giữ đúng như vậy:
- endpoint và method;
- request: field nào, kiểu primitive gì, bắt buộc hay không;
- response: status thành công, body;
- mọi lỗi: lỗi nào của core ánh xạ sang status nào.

So với hợp đồng hiện tại trong `docs/api.md`. Nếu đổi hành vi đang có (status, `detail`, field), nói rõ đây là thay đổi phá vỡ tương thích.

## 1. Domain (`internal/core/domain`), chỉ khi có quy tắc nghiệp vụ mới

- Invariant mới đặt trong constructor/method hoặc một value object; lỗi validate dùng `ValidationError` với tên field đúng như trong JSON.
- Test trong `*_test.go` cùng package `domain_test`.

## 2. Port (`internal/core/port/port.go`)

- Thêm method vào `UserService`. Input chỉ là primitive; nhiều tham số thì gom vào struct `XxxCmd`/`XxxQuery`.
- Cần storage mới thì thêm method vào `UserRepository` và **ghi hợp đồng lỗi** vào comment của interface (khi nào trả `ErrNotFound`, `ErrVersionConflict`, `domain.ErrEmailAlreadyExists`).

## 3. Repository: contract test trước

1. Thêm subtest vào `internal/adapter/outbound/repotest/contract.go`.
2. Chạy và thấy fail: `go test ./internal/adapter/outbound/memory/`.
3. Hiện thực trong `memory`, rồi `postgres` (SQL có tham số, dịch lỗi qua `mapError`; cần đổi schema thì dùng `/add-migration`). Lệnh **ghi** mới trong `postgres` đi qua `r.write` và gọi `r.appendEvent` khi thành công, kèm test trong `outbox_test.go` (xem `.claude/rules/messaging.md`).
4. Chạy cả hai, kèm PostgreSQL: `TEST_DATABASE_URL=... go test ./internal/adapter/outbound/...`

## 4. Service (`internal/core/service`)

1. Test trong `user_service_test.go`, dùng `newSUT()`; gồm cả ID sai định dạng và lỗi từ repository.
2. Hiện thực:
   - parse/validate input trước khi gọi repository (`parseUserID`);
   - wrap lỗi bằng `fmt.Errorf("...: %w", err)`;
   - log qua `s.logger`.

## 5. HTTP (`internal/adapter/inbound/httpapi`)

1. Test trong `user_handler_test.go`: status thành công, body, và mỗi lỗi ở bước 0 (status + `Content-Type: application/problem+json`). Tạo request bằng `NewRequestWithContext`.
2. Hiện thực:
   - route trong `server.go`;
   - handler trong `user_handler.go`, chỉ bind DTO → gọi port → map DTO;
   - lỗi mới của core thì thêm nhánh vào `toProblem` trong `errors.go`.

## 6. Tài liệu (cùng commit)

- `docs/api.md`: endpoint, request/response, thêm dòng vào bảng lỗi kèm nguyên văn `detail`.
- `api.http`: một request mẫu.
- Thêm port/adapter mới thì cập nhật `docs/architecture.md`.
- Lệnh ghi mới sinh loại sự kiện mới, hoặc đổi field của sự kiện, thì cập nhật `docs/events.md`.
- Kiểm chứng bằng server thật: `go run ./cmd/userapi`, gọi bằng `curl` từng tình huống ở bước 0, so status và body với những gì vừa ghi.

## 7. Hoàn tất

`TEST_DATABASE_URL=... make check` phải xanh. Không có PostgreSQL thì vẫn chạy `make check`, và nói rõ test PostgreSQL đã bị skip.

## Lỗi thường gặp

| Lỗi | Đúng là |
|---|---|
| Command của port nhận `domain.UserID` | Nhận `string`; service gọi `parseUserID` |
| Thêm `ErrXxxNotFound` mới | Dùng `port.ErrNotFound`, wrap kèm ngữ cảnh |
| Handler tự kiểm tra quy tắc nghiệp vụ | Chuyển xuống domain/service; handler chỉ bind và map |
| Chỉ sửa adapter postgres, quên memory | Contract test ở `repotest` bắt cả hai; thêm subtest trước |
| Sửa `.golangci.yml` vì depguard báo lỗi | Import sai hướng; sửa code |
| Ghi `detail` vào docs từ trí nhớ | Gọi API thật rồi chép nguyên văn |
