# API reference

Tài liệu cho người gọi API. Base URL mặc định khi chạy local: `http://localhost:8080`. Request mẫu có trong [`api.http`](../api.http).

## Quy ước chung

| Mục | Quy ước |
|---|---|
| Định dạng | JSON (`application/json`). Request có body (POST, PATCH) **phải** gửi `Content-Type: application/json` (không phân biệt hoa thường, được kèm tham số, ví dụ `; charset=utf-8`). Thiếu header hoặc mọi media type khác, kể cả form và XML, nhận 415 |
| Kích thước body | Tối đa 1 MiB, vượt quá trả 413 (kể cả body gửi chunked, không có `Content-Length`) |
| Field lạ trong body | Bị bỏ qua, không báo lỗi |
| ID | UUID v7 dạng chuẩn, chữ thường (`01a15359-aa28-7a00-a380-c47e52905707`). ID sai định dạng (kể cả UUID chữ hoa) được coi là không tồn tại: 404 |
| Thời gian | RFC 3339, UTC, độ chính xác micro giây (`2026-10-19T08:49:07.624656Z`). Số 0 ở cuối phần thập phân bị lược (`…07.5722Z`, `…07Z`), nên số chữ số thay đổi từ 0 tới 6: parse bằng bộ parse RFC 3339, đừng dùng định dạng cố định 6 chữ số |
| Lỗi | `application/problem+json` theo RFC 9457, xem [Lỗi](#lỗi) |
| Xác thực | Template **không có xác thực**. Xem [operations.md](operations.md#xác-thực) để gắn thêm |
| Timeout | Mỗi request có timeout `REQUEST_TIMEOUT` (mặc định 10s); mỗi câu SQL có timeout `DB_STATEMENT_TIMEOUT` (mặc định 5s). Hết hạn ở bên nào cũng trả 503 |

### Header

| Header | Chiều | Ý nghĩa |
|---|---|---|
| `X-Request-Id` | Request (tuỳ chọn) / Response (luôn có) | ID để đối chiếu log. Client có thể tự gửi; giá trị chỉ được giữ nếu dài tối đa 128 ký tự và chỉ gồm `A-Z a-z 0-9 - _ .`, nếu không server sinh ID mới. Response luôn trả lại ID đang dùng |
| `ETag` | Response của POST, GET một user, PATCH | Version hiện tại của user, dạng `"<version>"`, ví dụ `"3"` |
| `If-Match` | Request PATCH (tuỳ chọn) | Chỉ cập nhật nếu version hiện tại khớp. Xem [Cập nhật đồng thời](#cập-nhật-đồng-thời) |
| `Location` | Response của POST | Đường dẫn tới user vừa tạo |

## Tài nguyên `User`

```json
{
  "id": "01a15359-aa28-7a00-a380-c47e52905707",
  "email": "lan@example.com",
  "name": "Lan Nguyen",
  "created_at": "2026-10-19T08:49:07.624656Z",
  "updated_at": "2026-10-19T08:49:07.624656Z",
  "version": 1
}
```

| Field | Quy tắc |
|---|---|
| `email` | Bắt buộc; bỏ khoảng trắng hai đầu và chuyển chữ thường; tối đa 254 ký tự; phải là địa chỉ trần (`a@b.com`, không nhận `Tên <a@b.com>`); duy nhất trong hệ thống (so sánh sau khi chuẩn hoá) |
| `name` | Bắt buộc; khoảng trắng thừa (kể cả tab, xuống dòng, khoảng trắng Unicode) được gộp thành một dấu cách (`"  Lan   Nguyen "` → `"Lan Nguyen"`); tối đa 100 ký tự, đếm theo Unicode code point (một chữ viết bằng ký tự gốc + dấu tổ hợp tính là 2) |
| `version` | Bắt đầu từ 1, tăng 1 sau mỗi lần thay đổi thực sự |
| `updated_at` | Chỉ đổi khi dữ liệu thực sự thay đổi |

## Endpoint

### `POST /api/v1/users`: tạo user

```http
POST /api/v1/users
Content-Type: application/json

{"email": " Lan@Example.COM ", "name": "  Lan   Nguyen "}
```

**201 Created**, kèm `Location: /api/v1/users/<id>` và `ETag: "1"`. Body là user vừa tạo (email và name đã được chuẩn hoá).

Lỗi thường gặp: 422 (dữ liệu không hợp lệ), 409 (email đã tồn tại), 400 (body không phải JSON hợp lệ), 413 (body quá lớn), 415 (`Content-Type` không phải `application/json`).

### `GET /api/v1/users`: danh sách

| Query | Mặc định | Quy tắc |
|---|---|---|
| `limit` | 20 | `≤ 0` dùng 20; lớn hơn 100 được **giới hạn thành 100**, không báo lỗi; không phải số trả 400 |
| `offset` | 0 | Âm thì dùng 0; lớn hơn 10.000 trả 422; không phải số trả 400 |

Sắp xếp theo `created_at` tăng dần (rồi tới `id`). Response cho biết giá trị `limit`/`offset` **thực sự được áp dụng**:

```json
{
  "data": [ { "id": "…", "email": "lan@example.com", "name": "Lan Nguyen", "…": "…" } ],
  "meta": { "limit": 20, "offset": 0, "total": 42 }
}
```

Phân trang theo offset phù hợp với màn hình quản trị cần nhảy tới trang N. Không dùng để duyệt hết dữ liệu lớn (giới hạn offset 10.000 tồn tại vì lý do đó).

### `GET /api/v1/users/{id}`: lấy một user

**200 OK**, kèm `ETag`. Không tồn tại hoặc ID sai định dạng: 404.

### `PATCH /api/v1/users/{id}`: cập nhật từng phần

```http
PATCH /api/v1/users/01a15359-aa28-7a00-a380-c47e52905707
Content-Type: application/json
If-Match: "1"

{"name": "Lan Pham"}
```

- Chỉ field có mặt trong body mới được cập nhật. Field vắng mặt hoặc có giá trị `null` được giữ nguyên.
- Body rỗng hoặc `{}`: trả 200 với user hiện tại, không tăng `version`.
- **200 OK**, kèm `ETag` mới.

Lỗi thường gặp: 404, 422, 409 (email đã tồn tại, hoặc xung đột version), 412 (`If-Match` không khớp), 400, 413, 415 (như POST).

### `DELETE /api/v1/users/{id}`: xoá

**204 No Content**. Không tồn tại hoặc ID sai định dạng: 404. Email của user đã xoá có thể dùng lại.

> `DELETE` **không** kiểm tra `If-Match`: header này bị bỏ qua và user vẫn bị xoá.

### Probe

| Endpoint | Ý nghĩa | Response |
|---|---|---|
| `GET /healthz` | Liveness: tiến trình còn sống | 200 `{"status":"ok"}` |
| `GET /readyz` | Readiness: pod sẵn sàng nhận traffic | 200 `{"status":"ready"}`; 503 khi đang shutdown (hoặc database không phản hồi, nếu bật `READINESS_CHECK_DB`) |

Probe không có tiền tố `/api/v1`, không đi qua middleware xác thực và không sinh dòng log truy cập `http_request`. `/readyz` trả 503 thì server ghi một dòng WARN `readiness check failed`.

## Cập nhật đồng thời

Mỗi lần ghi kiểm tra `version` (optimistic locking), nên hai PATCH chạy song song không bao giờ âm thầm ghi đè thay đổi của nhau.

| Client gửi | Khi có xung đột |
|---|---|
| Không có `If-Match` (hoặc `If-Match: *`) | Server tự đọc lại và áp dụng lại thay đổi, tổng cộng tối đa 3 lần thử. Vẫn xung đột thì trả **409** |
| `If-Match: "<version>"` | Version hiện tại khác thì trả **412**, không thử lại. Client nên GET lại, cho người dùng xem dữ liệu mới, rồi quyết định |
| `If-Match` là weak ETag (`W/"2"`) hoặc không đúng nguyên văn dạng `"<số>"` mà server cấp (ví dụ `"+2"`, `"02"`) | Luôn trả **412** (theo RFC 9110, `If-Match` dùng so sánh strong: so từng ký tự) |
| `If-Match` rỗng, hoặc nhiều ETag (danh sách `"1", "2"` hay nhiều dòng header) | Luôn trả **412**, kể cả khi danh sách có version hiện tại. Server chỉ nhận **một** ETag; từ chối an toàn hơn là coi như không có `If-Match` rồi ghi đè vô điều kiện |

Client hiển thị form sửa cho người dùng nên lưu `ETag` từ lúc GET và gửi lại trong `If-Match`. Đó là cách duy nhất phát hiện việc người dùng sửa trên dữ liệu đã cũ.

## Lỗi

Mọi lỗi trả `Content-Type: application/problem+json`:

```json
{
  "type": "about:blank",
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "request failed validation",
  "instance": "/api/v1/users",
  "errors": [
    {"field": "email", "message": "must be a valid email address"},
    {"field": "name",  "message": "is required"}
  ]
}
```

`errors` chỉ có ở lỗi 422. Lỗi 5xx không bao giờ chứa chi tiết nội bộ; dùng `X-Request-Id` để tìm lỗi gốc trong log.

| Status | Khi nào | `detail` |
|---|---|---|
| 400 | Body không phải JSON hợp lệ | `request body is not valid JSON` |
| 400 | Field sai kiểu, ví dụ `{"email": 5}` | `field "email" has the wrong type` |
| 400 | Body là JSON nhưng không phải object, ví dụ `[]` | `request body must be a JSON object` |
| 400 | `limit`/`offset` không phải số nguyên | `limit must be an integer` / `offset must be an integer` |
| 404 | User không tồn tại, hoặc ID sai định dạng | `resource not found` |
| 404 | Route không tồn tại (kể cả có `/` ở cuối, như `/api/v1/users/`) | (trống) |
| 405 | Sai method cho route; response có header `Allow` | |
| 409 | Email đã thuộc về user khác | `a user with this email already exists` |
| 409 | Xung đột version sau 3 lần thử (không có `If-Match`) | `the user was modified concurrently, please retry` |
| 412 | `If-Match` không khớp version hiện tại | `If-Match does not match the current version of the resource` |
| 413 | Body lớn hơn 1 MiB | |
| 415 | POST/PATCH có body nhưng `Content-Type` không phải `application/json` (thiếu, form, XML...) | |
| 422 | Dữ liệu không hợp lệ, kèm `errors` theo từng field | `request failed validation` |
| 499 | Client đóng kết nối trước khi có response, hoặc request bị huỷ khi server tắt quá hạn. Client không nhận được response này; chỉ thấy status 499 trong log | |
| 500 | Lỗi không lường trước | (trống) |
| 503 | Request vượt `REQUEST_TIMEOUT`, hoặc câu SQL vượt `DB_STATEMENT_TIMEOUT` | `request timed out` |
| 503 | `/readyz` khi pod chưa sẵn sàng | (trống) |

Thông báo lỗi theo field (`errors[].message`):

| Field | Message |
|---|---|
| `email` | `is required`, `must be at most 254 characters`, `must be a valid email address` |
| `name` | `is required`, `must be at most 100 characters` |
| `offset` | `must be at most 10000` |
