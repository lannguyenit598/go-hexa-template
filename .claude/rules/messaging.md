---
paths:
  - "internal/outbox/**/*.go"
  - "internal/adapter/outbound/kafka/**/*.go"
  - "internal/adapter/outbound/postgres/outbox*.go"
  - "internal/app/relay.go"
  - "docs/events.md"
---

# Events and message brokers

- Events go only through the outbox: write them in the same transaction as data (`r.write` + `r.appendEvent` in postgres). Never publish directly from HTTP requests: committing data before a failed publish loses the event.
- Core is unaware of events: `git diff main -- internal/core` must be empty for event-only changes. If an event needs service-only information (actor, reason, old values), first read "Publishing events through a transactional outbox" in `docs/architecture.md`.
- Keep events thin: no email, name, or other personal data in payloads, headers, logs, or traces.
- Delivery is at-least-once. Never promise exactly-once delivery in code or docs; consumers deduplicate by `event_id` or `version`.
- Per-user event `version` increases by exactly 1 (`user.deleted` = last version + 1). New events must preserve this for stale-event filtering and gap detection.
- Event headers carry only trace context (`propagation.TraceContext`), not the global propagator, which includes client-controlled Baggage.
- `Sink.Publish` returns per-message errors. Wrap `outbox.ErrPermanent` only when the broker rejects that message's content or size; network, topic, and permission errors are temporary. Misclassification can wrongly park events or block progress indefinitely.
- Schema (`docs/events.md`): only add fields within one `schema_version`. Renaming, changing types, or removing fields requires a version bump. Update `docs/events.md` in the same commit for schema or event-type changes.
- `internal/outbox` knows only `Source`/`Sink` and uses only the standard library (depguard `outbox`): no adapters, `pgx`, `franz-go`, or domain imports. A new broker needs an `outbox.Sink` adapter and a `sinkModule` branch in `internal/app/relay.go`.
- Kafka tests use in-memory `kfake`, not a real broker. PostgreSQL outbox tests use `pgtest.FreshSchemaDSN` and migrate their own schema.
