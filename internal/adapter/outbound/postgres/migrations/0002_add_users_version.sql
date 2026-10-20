-- +goose Up
-- Expand change: add a column with a DEFAULT so old code unaware of version keeps working
-- during rolling deployments. PostgreSQL 11+ adds columns with constant defaults
-- without rewriting the table.
ALTER TABLE users ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS version;
