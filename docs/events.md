# Events

Reference for userapi event consumers and publishing operators. Design: [architecture.md](architecture.md#publishing-events-through-a-transactional-outbox).

userapi publishes an event for each **successful** user creation, update, or deletion. Disabled by default (`EVENTS_SINK=none`); enable with `EVENTS_SINK=kafka`, requiring `STORAGE=postgres` ([operations](operations.md#event-publishing)).

## Flow

1. The API writes the user change and outbox row **in one transaction**: both persist or neither does. Once **every** API instance runs with events enabled, a committed change cannot lack its event; changes committed by instances without `EVENTS_SINK` have none ([enabling events](operations.md#event-publishing)).
2. `userapi relay` reads batches, publishes to Kafka, and marks delivered rows.
3. During broker outages the API continues operating; events wait in the outbox until recovery.

Unchanged PATCHes and failed operations (404, 409, 412, 422) do not emit events.

## Event types

| `event_type` | When | `version` |
|---|---|---|
| `user.registered` | Successful `POST /api/v1/users` | 1 |
| `user.updated` | PATCH changes data | New version |
| `user.deleted` | Successful DELETE | Last version before deletion + 1 |

## Kafka message

| Part | Content |
|---|---|
| Topic | `KAFKA_TOPIC` (default `users`) |
| Key | `user_id`: a user's events share one partition |
| Header `event_id` | Same as `event_id` in the value |
| Header `event_type` | Same as `event_type` in the value |
| Headers `traceparent`, `tracestate` | W3C trace context from the triggering HTTP request, if present, for consumer trace continuation. Request `baggage` is **not** forwarded because clients can set arbitrary values |
| Value | JSON using the schema below |

```json
{
  "schema_version": 1,
  "event_id": "01a15769-9e14-78f7-9080-6656da749773",
  "event_type": "user.updated",
  "occurred_at": "2026-10-20T03:45:01.971643Z",
  "user_id": "01a15769-9e05-7878-b85c-d99cd12c3c61",
  "version": 2
}
```

| Field | Meaning |
|---|---|
| `schema_version` | Schema version, currently `1` |
| `event_id` | Unique UUIDv7 for each event |
| `event_type` | One of the three types above |
| `occurred_at` | RFC 3339, UTC. Creation/update: user change time (`created_at`/`updated_at`); deletion: deletion time |
| `user_id` | User ID |
| `version` | User version; see event types |

**Thin events omit email and name.** Consumers needing details call `GET /api/v1/users/{user_id}` (404 means deleted). This avoids retaining copies of those fields on Kafka; identifiers and retention still need the project's own data-handling assessment.

## Consumer guarantees

- **`version` increases by exactly 1 per user event:** registered starts at 1, updated increments by 1, deleted is last version + 1. Consumers can use it as follows:
  - version no greater than the user's last processed version: duplicate or stale; ignore;
  - version greater than last processed + 1: an intermediate event has not arrived (out of order or parked; see below). Fetch current state with `GET /api/v1/users/{user_id}`.
  - Gaps only reveal missing events for users a consumer has heard of. Changes committed while events were only partly enabled produce no event at all: a user created, or created and deleted, in that window never appears to consumers.
- **At-least-once delivery.** A relay can resend an event after crashing between broker acceptance and outbox marking. Deduplicate by version as above or by event_id.
- **Per-user ordering.** A single relay sends events in commit order. Concurrent relays and partial batch rejections followed by retries can reorder events. Version-aware consumers discard late arrivals with smaller versions.
- **Permanent broker rejections**, such as oversized records, are parked instead of blocking subsequent events. Consumers receive them only after operator intervention ([operations](operations.md#event-publishing)); version gaps reveal missing events.
- **Backward compatibility within a schema_version.** Only *add* fields: no renames, type changes, or removals. Consumers ignore unknown fields. Breaking changes increment schema_version.

## Adding event types

Events originate in the postgres adapter (`internal/adapter/outbound/postgres/outbox.go`), not core: one per successful Create/Update/Delete when enabled. For another repository-write event, add an `eventUser...` constant and appendEvent call in the transaction, test in outbox_test.go, and update the table. Events needing service-only context (actor, reason, old values) require domain events; see [architecture](architecture.md#publishing-events-through-a-transactional-outbox).
