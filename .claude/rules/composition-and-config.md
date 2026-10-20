---
paths:
  - "cmd/**/*.go"
  - "internal/app/**/*.go"
  - "internal/config/**/*.go"
  - "internal/observability/**/*.go"
  - ".env.example"
  - "docker-compose.yml"
  - "Dockerfile"
---

# Composition root, configuration, and shared infrastructure

- `cmd/userapi/main.go` only reads config, creates a logger, handles signals, selects a command, and calls `app.Run`/`app.Migrate`/`app.RunRelay`. Keep lifecycle logic in `internal/app`, where it is tested.
- `internal/app` is the only place that wires adapters into core (`newRepository`, `httpapi.NewServer`, `newSink`). Every resource opened in `Run` must be closed on **every** startup error path.
- Preserve shutdown order: mark draining → wait `ShutdownDelay` → `srv.Shutdown` → close pool → flush telemetry. If shutdown times out, cancel the root request context (`BaseContext`) and call `srv.Close()` **before** closing the pool; otherwise `pool.Close()` waits for queries and exceeds the shutdown budget. Rerun `internal/app/app_test.go` when changing this.
- `newRepository` uses a `switch` with an error-returning `default`: each `Storage` value needs its own branch.
- Add environment variables completely in the same commit:
  1. read and validate in `internal/config/config.go`, collecting errors through `parser`;
  2. add default and invalid-value cases in `config_test.go`;
  3. add to `.env.example`;
  4. document default, meaning, and constraints in `docs/operations.md`.
- `internal/config` and `internal/observability` must not import core, adapters, or `internal/app` (depguard `platform` rule).
- Network I/O requires context: `net.ListenConfig{}.Listen(ctx, …)`, `http.NewRequestWithContext` (`noctx` lint).
