-- +goose Up
-- Transactional outbox: events and data changes share a transaction; the relay
-- (`userapi relay`) forwards them to the broker. This new table is safe for rolling deployments:
-- old code does not use it. aggregate_id has no users foreign key because user.deleted events
-- must survive deletion of the user.
CREATE TABLE IF NOT EXISTS outbox (
    id           UUID        PRIMARY KEY,
    aggregate_id UUID        NOT NULL,
    event_type   TEXT        NOT NULL,
    -- JSON, not JSONB: preserve the adapter's exact bytes for delivery to the broker.
    payload      JSON        NOT NULL,
    headers      JSONB       NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ
);

-- Relay reads unpublished rows in write order: the partial index stays small as the table grows.
CREATE INDEX IF NOT EXISTS outbox_unpublished_idx ON outbox (created_at, id) WHERE published_at IS NULL;
-- Purge published rows older than OUTBOX_RETENTION.
CREATE INDEX IF NOT EXISTS outbox_published_at_idx ON outbox (published_at) WHERE published_at IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS outbox;
