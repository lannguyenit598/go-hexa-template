---
paths:
  - "internal/core/**/*.go"
---

# Core (domain, port, service)

- Import only the standard library and `internal/core/...`. For external capabilities (time, IDs, storage, email...), define an interface in `port` and implement it in an adapter.
- `domain`:
  - Preserve invariants through constructors and methods (`NewUser`, `Apply`). Use value objects such as `Email` for values with their own rules.
  - Storage reconstruction (`EmailFromStore`) must **not** revalidate, so tighter rules do not break reads of legacy data.
  - `Apply` is pure: return a copy, changing `Version`/`UpdatedAt` only when data changes.
  - No JSON/DB tags.
- `port`:
  - Inbound commands/queries contain primitives; outbound interfaces use domain types.
  - Shared use-case outcome errors (`ErrNotFound`, `ErrVersionConflict`) belong here; do not create aggregate-specific `ErrXxxNotFound` errors.
  - Every `UserRepository` contract change needs a test in `internal/adapter/outbound/repotest`.
- `service`:
  - Validate/parse input (such as `parseUserID`) **before** repository calls.
  - Wrap errors with context using `%w`.
  - Log through injected `s.logger`, not `slog.Default()`/`slog.Info(...)` (`forbidigo` blocks these and `time.Now`).
  - Retry only safely repeatable operations (such as `Update`: read again and call `Apply` again).
- Service tests use `fakeClock`/`seqIDs` and in-memory repositories; simulate contention with a repository wrapper (see `racingRepo`).
