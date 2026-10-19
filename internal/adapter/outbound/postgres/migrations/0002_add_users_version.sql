-- +goose Up
-- Thay đổi kiểu "expand": thêm cột có DEFAULT, nên code cũ (không biết cột version) vẫn
-- chạy được trong lúc rolling deploy. PostgreSQL 11+ thêm cột có DEFAULT hằng số mà
-- không phải ghi lại bảng.
ALTER TABLE users ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS version;
