-- 0001_create_users.sql
-- Idempotent: chạy lại nhiều lần không lỗi.
CREATE TABLE IF NOT EXISTS users (
    id         UUID        PRIMARY KEY,
    email      TEXT        NOT NULL,
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS users_email_key ON users (email);
CREATE INDEX IF NOT EXISTS users_created_at_id_idx ON users (created_at, id);
