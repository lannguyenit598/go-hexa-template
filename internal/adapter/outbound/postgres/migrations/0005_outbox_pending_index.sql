-- +goose NO TRANSACTION
-- +goose Up
-- Parked rows keep published_at NULL and are never purged, so outbox_unpublished_idx (0003) keeps
-- them, and they sort first as the oldest unpublished rows. This index excludes them, matching the
-- relay's pending query (published_at IS NULL AND parked_at IS NULL). CONCURRENTLY does not block
-- writes; it must run outside a transaction, alone in its file. 0006 drops the old index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_pending_idx ON outbox (created_at, id)
    WHERE published_at IS NULL AND parked_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS outbox_pending_idx;
