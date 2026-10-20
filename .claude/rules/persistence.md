---
paths:
  - "internal/adapter/outbound/**/*.go"
  - "internal/adapter/outbound/postgres/migrations/*.sql"
---

# Outbound adapters and persistence

- Outbound adapters are independent: no other outbound adapters, inbound adapters, Echo, or `internal/app` imports (enforced by depguard). Add new adapter paths to `outbound-adapters` in `.golangci.yml`.
- Repositories must satisfy the `port.UserRepository` comment contract and **pass `repotest.Run`**. Add new behavior to the shared suite first so every adapter is checked.
- Return bare sentinels (`port.ErrNotFound`, `port.ErrVersionConflict`, `domain.ErrEmailAlreadyExists`). Translate driver errors in `mapError`; never leak `pgx`/`pgconn`. Map database timeouts (`57014`, `statement_timeout`) to `context.DeadlineExceeded` for HTTP 503.
- `Update` uses compare-and-set on `version` in **one** `UPDATE ... WHERE id = $1 AND version = $n`, without `SELECT ... FOR UPDATE`. With outbox enabled, the update and event insert share a short transaction (`r.write`).
- New repository writes use `r.write` and call `r.appendEvent` on success so events commit with data (see `.claude/rules/messaging.md`).
- Parameterize SQL (`$1`); do not concatenate values.
- Traces must not contain parameter values (personal data): keep default `otelpgx` settings without `WithIncludeQueryParameters`.
- Migrations: **never change executable SQL in a merged migration**; add a new file. Use `/add-migration`. Comment-only translations must preserve SQL and goose directives.
- Runtime migration checks must be read-only. `goose.HasPending` creates `goose_db_version`, so check `to_regclass` first as in `PendingMigrations`.
