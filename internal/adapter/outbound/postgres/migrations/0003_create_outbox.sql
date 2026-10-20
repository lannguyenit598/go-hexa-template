-- +goose Up
-- Transactional outbox: sự kiện được ghi cùng transaction với thay đổi dữ liệu, rồi relay
-- (`userapi relay`) đẩy lên broker. Bảng mới, nên an toàn khi rolling deploy: code cũ không biết
-- tới nó. aggregate_id không có foreign key tới users vì sự kiện user.deleted phải còn sau khi
-- user đã bị xoá.
CREATE TABLE IF NOT EXISTS outbox (
    id           UUID        PRIMARY KEY,
    aggregate_id UUID        NOT NULL,
    event_type   TEXT        NOT NULL,
    -- JSON (không phải JSONB): giữ nguyên từng byte adapter đã ghi, để broker nhận đúng bản đó.
    payload      JSON        NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);

-- Relay chỉ đọc bản ghi chưa publish, theo thứ tự ghi: partial index giữ nhỏ dù bảng lớn.
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox (created_at, id) WHERE published_at IS NULL;
-- Dọn bản ghi đã publish quá OUTBOX_RETENTION.
CREATE INDEX IF NOT EXISTS outbox_published_at_idx ON outbox (published_at) WHERE published_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS outbox;
