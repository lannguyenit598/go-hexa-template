# Spec: Hexagonal Architecture Refinements (Review Items 1–5, 2026-10-19)

Source: architecture review after seven commits on claude/vibrant-keller-k2m8al.
Scope: items 1–5 only. ADR-001 (bounded-context package organization) is **out of scope**;
implement only when a second aggregate exists.

## R1. Thin composition root with tested lifecycle
- cmd/userapi/main.go only reads config, creates a logger, handles signals, and selects the default or migrate command.
- Move adapter creation, readiness, shutdown drain, HTTP server timeouts, telemetry flushing,
  and pending-migration checks into internal/app, with automated tests for:
  - after cancellation during SHUTDOWN_DELAY: /readyz=503 while API remains 200;
  - after shutdown: listener closed and Run returns nil;
  - startup failure such as an occupied port returns an error without hanging;
  - pending migrations, including an entirely empty database, cause an error containing
    pending migrations; MIGRATE_ON_START=true permits normal startup.
- Preserve immediate termination on the second signal.

## R2. Primitive-only inbound inputs
- port.UserService commands/queries contain only primitives (string, *string, int64...);
  no domain.UserID or domain.Changes.
- Services convert to domain types and validate IDs; malformed IDs return not found.
- Results remain domain.User, which adapters may read.
- Preserve HTTP statuses and JSON.

## R3. No global service logger
- Inject *slog.Logger into service.UserService; nil discards logs.
- Version-conflict retry logs use the injected logger and *Context(ctx, ...).

## R4. Close depguard gaps
- internal/observability and internal/config cannot import adapters, core, or app.
- Outbound adapters cannot import one another or repotest.
- No package outside cmd imports internal/app.

## R5. Application owns use-case outcome errors
- Move ErrNotFound and ErrVersionConflict from domain to port, shared by all aggregates
  without ErrUserNotFound. Add context through fmt.Errorf("user %s: %w", ...).
- Domain retains only business errors: ValidationError and ErrEmailAlreadyExists.
- Preserve HTTP mapping: not found=404, version conflict=409 (412 with If-Match).
