-- +goose Up
-- Permanently rejected messages (for example, oversized ones) are parked rather than blocking
-- later messages: the relay stores parked_at and the reason, then stops selecting them. Adding
-- only nullable columns is safe for rolling deployments; old relays still work.
ALTER TABLE outbox
    ADD COLUMN IF NOT EXISTS parked_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_error TEXT;

-- +goose Down
ALTER TABLE outbox
    DROP COLUMN IF EXISTS last_error,
    DROP COLUMN IF EXISTS parked_at;
