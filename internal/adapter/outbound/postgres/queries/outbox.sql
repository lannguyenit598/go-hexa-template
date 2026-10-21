-- name: InsertOutboxEvent :exec
-- created_at uses clock_timestamp() (INSERT time), not now() (transaction start time).
INSERT INTO outbox (id, aggregate_id, event_type, payload, headers, created_at)
VALUES ($1, $2, $3, $4, $5, clock_timestamp());

-- name: LockPendingOutboxEvents :many
-- FOR UPDATE SKIP LOCKED: concurrent relays neither select the same row nor wait for each other.
SELECT id, aggregate_id, event_type, payload, headers FROM outbox
WHERE published_at IS NULL AND parked_at IS NULL
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows)::bigint
FOR UPDATE SKIP LOCKED;

-- name: MarkOutboxEventsPublished :exec
UPDATE outbox SET published_at = now() WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ParkOutboxEvent :exec
UPDATE outbox SET parked_at = now(), last_error = sqlc.arg(last_error)::text WHERE id = @id;

-- name: PurgePublishedOutboxEvents :execrows
-- Parked and unpublished rows have a NULL published_at and are never purged. The casts keep
-- parameters non-nullable: the columns are nullable, so sqlc would otherwise use pgtype types.
-- At most max_rows per call keeps each DELETE short; the relay repeats it in bounded turns.
DELETE FROM outbox WHERE id IN (
  SELECT id FROM outbox WHERE published_at < sqlc.arg(before)::timestamptz
  ORDER BY published_at LIMIT sqlc.arg(max_rows)::bigint
);
