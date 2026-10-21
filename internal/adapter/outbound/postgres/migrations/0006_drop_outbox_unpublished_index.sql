-- +goose NO TRANSACTION
-- +goose Up
-- outbox_pending_idx (0005) replaces this index. Relays from 0004 on already filter
-- parked_at IS NULL, so they use the new index during rolling deployments and rollbacks.
DROP INDEX CONCURRENTLY IF EXISTS outbox_unpublished_idx;

-- +goose Down
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_unpublished_idx ON outbox (created_at, id) WHERE published_at IS NULL;
