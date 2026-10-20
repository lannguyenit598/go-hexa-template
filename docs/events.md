# Sự kiện

Tài liệu cho service tiêu thụ sự kiện của `userapi` và cho người vận hành phần phát sự kiện. Kiến trúc và lý do thiết kế: [architecture.md](architecture.md#phát-sự-kiện-qua-transactional-outbox).

`userapi` phát một sự kiện mỗi khi user được tạo, sửa hoặc xoá **thành công**. Tính năng tắt mặc định (`EVENTS_SINK=none`); bật bằng `EVENTS_SINK=kafka`, cần `STORAGE=postgres` (xem [operations.md](operations.md#phát-sự-kiện)).

## Luồng

1. API ghi thay đổi của user và một bản ghi vào bảng `outbox` **trong cùng một transaction**: hoặc cả hai cùng được lưu, hoặc không cái nào. Không bao giờ có thay đổi dữ liệu mà thiếu sự kiện.
2. Tiến trình `userapi relay` đọc outbox theo lô, gửi lên topic Kafka, rồi đánh dấu bản ghi đã gửi.
3. Broker không tới được thì API vẫn chạy bình thường; sự kiện nằm chờ trong outbox và được gửi khi broker trở lại.

Lần ghi không đổi dữ liệu (PATCH với giá trị như cũ) hoặc thất bại (404, 409, 412, 422) không sinh sự kiện.

## Loại sự kiện

| `event_type` | Khi nào | `version` |
|---|---|---|
| `user.registered` | `POST /api/v1/users` thành công | 1 |
| `user.updated` | `PATCH` làm thay đổi dữ liệu | Version mới sau thay đổi |
| `user.deleted` | `DELETE` thành công | Version cuối cùng trước khi xoá + 1 |

## Message trên Kafka

| Phần | Nội dung |
|---|---|
| Topic | `KAFKA_TOPIC` (mặc định `users`) |
| Key | `user_id`: mọi sự kiện của một user nằm trên cùng partition |
| Header `event_id` | Như `event_id` trong value |
| Header `event_type` | Như `event_type` trong value |
| Header `traceparent`, `tracestate` | Trace context (W3C) của request HTTP đã gây ra sự kiện, nếu có. Consumer dùng nó để nối trace. Header `baggage` của request **không** được chuyển tiếp, vì client tự đặt được giá trị tuỳ ý trong đó |
| Value | JSON, schema bên dưới |

```json
{
  "schema_version": 1,
  "event_id": "01a15769-9e14-78f7-9080-6656da749773",
  "event_type": "user.updated",
  "occurred_at": "2026-10-20T03:45:01.971643Z",
  "user_id": "01a15769-9e05-7878-b85c-d99cd12c3c61",
  "version": 2
}
```

| Field | Ý nghĩa |
|---|---|
| `schema_version` | Phiên bản schema, hiện là `1` |
| `event_id` | UUIDv7, duy nhất cho mỗi sự kiện |
| `event_type` | Một trong ba loại ở trên |
| `occurred_at` | RFC 3339, UTC. Tạo/sửa: thời điểm thay đổi trên user (`created_at`/`updated_at`); xoá: thời điểm xoá |
| `user_id` | ID của user |
| `version` | Version của user, xem bảng loại sự kiện |

**Sự kiện mỏng, không có dữ liệu cá nhân.** Value không chứa email hay tên. Consumer cần chi tiết thì gọi `GET /api/v1/users/{user_id}` (404 nghĩa là user đã bị xoá). Nhờ vậy xoá một user không đòi hỏi xoá gì trên Kafka, và retention của topic không phải là vấn đề pháp lý.

## Cam kết với consumer

- **`version` tăng đúng 1 qua mỗi sự kiện của một user:** `user.registered` là 1, mỗi `user.updated` tăng 1, `user.deleted` là version cuối + 1. Consumer nên dựa vào nó:
  - `version` không lớn hơn version đã xử lý của user đó: bản trùng hoặc bản cũ, bỏ qua;
  - `version` lớn hơn version đã xử lý + 1: có sự kiện ở giữa chưa tới (đến lệch thứ tự, hoặc bị tách riêng, xem dưới). Gọi `GET /api/v1/users/{user_id}` để lấy trạng thái hiện tại.
- **Giao ít nhất một lần.** Relay có thể gửi lại một sự kiện, ví dụ khi tiến trình chết sau khi Kafka đã nhận nhưng trước khi đánh dấu outbox. Bỏ trùng theo `version` như trên, hoặc theo `event_id`.
- **Thứ tự theo từng user.** Với một relay, sự kiện của một user được gửi theo thứ tự commit. Có hai trường hợp hiếm làm lệch thứ tự: chạy nhiều relay song song, và một lô bị broker từ chối một phần rồi được gửi lại. Consumer dựa vào `version` không bị ảnh hưởng: sự kiện đến muộn có version nhỏ hơn bị bỏ qua.
- **Sự kiện bị broker từ chối vĩnh viễn** (ví dụ lớn hơn giới hạn của topic) được tách riêng thay vì chặn mọi sự kiện phía sau. Consumer sẽ không nhận sự kiện đó cho tới khi người vận hành xử lý (xem [operations.md](operations.md#phát-sự-kiện)); nhờ `version`, consumer thấy được khoảng trống.
- **Tương thích ngược trong cùng `schema_version`.** Chỉ *thêm* field; không đổi tên, không đổi kiểu, không xoá field. Consumer phải bỏ qua field không biết. Thay đổi không tương thích sẽ tăng `schema_version`.

## Thêm loại sự kiện

Sự kiện được sinh trong adapter postgres (`internal/adapter/outbound/postgres/outbox.go`), không phải trong core: mỗi lần `Create`/`Update`/`Delete` thành công sinh đúng một sự kiện. Sự kiện mới có cùng tính chất (gắn với một lần ghi của repository) thì thêm hằng `eventUser...` và lời gọi `appendEvent` trong cùng transaction, kèm test trong `outbox_test.go`, rồi cập nhật bảng ở trên. Sự kiện cần thông tin chỉ service biết (ai thực hiện, lý do, giá trị cũ) thì phải chuyển sang mô hình sự kiện trong domain; xem [architecture.md](architecture.md#phát-sự-kiện-qua-transactional-outbox).
