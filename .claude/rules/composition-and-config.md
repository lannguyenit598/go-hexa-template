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
- `internal/app` is the only place that wires adapters into core, through the Uber Fx graph (`apiGraph`, `relayGraph`, `storageModule`, `sinkModule`); only `internal/app` imports fx (depguard). Keep `Run`/`RunRelay` signatures; call `app.Start`/`app.Stop`, never `(*fx.App).Run` (own signals, `os.Exit`). `TestGraphs_ResolveEveryDependency` must cover every graph variant.
- Constructors do no network I/O: open, ping, and migrate in `OnStart`, and release in `OnStart` itself when it fails, because fx runs `OnStop` only for hooks that started. Every resource must be closed on **every** startup error path, with one accepted exception: clients whose constructors start goroutines (`pgxpool`, `kgo`) are not closed when a later constructor fails inside `fx.New`. Only startup fails this way and the process exits right after; keep fallible constructor steps before allocation. Start hooks that may wait long derive their context with `abortOnRun` so a signal during startup aborts them.
- Hook order is registration order (`OnStop` reverse), which follows dependency resolution and `fx.Invoke` order, not `fx.Provide` order; child modules' invokes run before the parent's (so telemetry is `fx.Module("telemetry", ...)`, first). `TestRun_HookOrder`/`TestRunRelay_HookOrder` pin the order: update them with any graph change. Long-lived contexts (`BaseContext`, relay loop) never derive from the `OnStart` context.
- Preserve shutdown order: mark draining → wait `ShutdownDelay` → `srv.Shutdown` → close pool → flush telemetry (one drain `OnStop` in `newHTTPServer`; the pool and telemetry hooks register earlier). If shutdown times out, cancel the root request context (`BaseContext`) and call `srv.Close()` **before** closing the pool; otherwise `pool.Close()` waits for queries and exceeds the shutdown budget. `app.Stop` gets no deadline because fx skips the remaining hooks once it expires; each hook bounds itself. Rerun `internal/app/app_test.go` with `TEST_DATABASE_URL` when changing this.
- `storageModule`/`sinkModule` use a `switch` with an `fx.Error` `default`: each `Storage`/`EventsSink` value needs its own branch.
- Add environment variables completely in the same commit:
  1. read and validate in `internal/config/config.go`, collecting errors through `parser`;
  2. add default and invalid-value cases in `config_test.go`;
  3. add to `.env.example`;
  4. document default, meaning, and constraints in `docs/operations.md`.
- `internal/config` and `internal/observability` must not import core, adapters, or `internal/app` (depguard `platform` rule).
- Network I/O requires context: `net.ListenConfig{}.Listen(ctx, …)`, `http.NewRequestWithContext` (`noctx` lint).
