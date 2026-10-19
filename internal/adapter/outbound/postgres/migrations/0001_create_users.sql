-- +goose Up
-- IF NOT EXISTS: database đã được tạo bằng cơ chế migration cũ (trước goose) vẫn chạy
-- được file này mà không lỗi; goose sẽ chỉ ghi nhận version.
CREATE TABLE IF NOT EXISTS users (
    id         UUID        PRIMARY KEY,
    email      TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS users_email_key ON users (email);
CREATE INDEX IF NOT EXISTS users_created_at_id_idx ON users (created_at, id);

-- +goose Down
DROP TABLE IF EXISTS users;
