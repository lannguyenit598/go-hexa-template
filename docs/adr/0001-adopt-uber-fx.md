# ADR-0001: Adopt uber-go/fx in the composition root

**Status:** Accepted
**Date:** 2026-10-21
**Deciders:** Repository maintainers

## Context

`internal/app` wires adapters into core by hand: `Run` (API, `app.go`) and `RunRelay` (relay, `relay.go`) construct components in sequence and release them with `defer` in reverse order. `Migrate` is a one-shot command with no object graph. The graph is small today (telemetry, repository/pool, `service.UserService`, Echo server; telemetry, sink, pool, relay, health server), but the team has decided to standardize on `go.uber.org/fx` for dependency injection and lifecycle hooks.

The current wiring carries invariants that fx must not silently break:

- `cmd/userapi/main.go` owns signals (`signal.NotifyContext`, second signal kills) and the exit code; `Run`/`RunRelay` return errors instead of exiting.
- Shutdown order (`.claude/rules/composition-and-config.md`): mark draining → wait `ShutdownDelay` → `srv.Shutdown` (bounded by `ShutdownTimeout`) → on timeout cancel `BaseContext` and `srv.Close()` → close pool → flush telemetry. Pool close and telemetry flush always run.
- Every resource opened during startup is closed on every startup error path.
- A runtime failure (`srv.Serve` error, `relay.Run` error) becomes the return value and therefore a non-zero exit.
- The relay checks the broker before connecting to PostgreSQL so a bad sink is reported first.
- Logs are JSON through the injected `*slog.Logger`.

These invariants are pinned by `internal/app/app_test.go` (11 tests against `app.Run`/`app.RunRelay`).

## Decision

Use `go.uber.org/fx` v1.24.0 **only inside `internal/app`**, behind the existing function signatures:

1. **Keep `app.Run(ctx, cfg, logger, opts)` and `app.RunRelay(...)`.** Each builds an `fx.App`, calls `app.Start(startCtx)`, waits for `ctx.Done()` or a runtime error, then calls `app.Stop(stopCtx)`. Never call `(*fx.App).Run()`: it installs its own signal handling and calls `os.Exit` on failure, which bypasses `main` and kills the test process. `cmd/userapi` stays unchanged and never imports fx.
2. **`Migrate` stays a plain function.** It has no graph.
3. **Core and adapters stay fx-free.** No `fx.In`/`fx.Out`, `fx.Lifecycle`, or annotations outside `internal/app`; interface binding (`fx.Annotate(..., fx.As(new(port.UserRepository)))`, `port.Clock`, `port.IDGenerator`) is declared in `internal/app`. depguard enforces this.
4. **Open resources in `OnStart`, not in constructors.** `postgres.Connect` is split into `NewPool` (no network check) and `Ping`; `Connect` keeps its behavior for adapter tests. Kafka `NewSink` does no network I/O; its ping moves to `OnStart`. Accepted exception: `pgxpool.NewWithConfig` and `kgo.NewClient` start background goroutines in their constructors, so if a later constructor fails inside `fx.New` (for example an unparsable `DATABASE_URL` after the relay's Kafka client exists), nothing closes them. Only startup fails this way and the process exits right after; keep every fallible constructor step before resource allocation where practical.
5. **No fx-level deadlines; each hook bounds itself.**
   - Stop: `app.Stop(context.Background())`. Verified in `fx@v1.24.0/internal/lifecycle/lifecycle.go` (`Stop`): fx keeps running remaining hooks after a hook **error**, but returns immediately once the stop **context expires**, skipping pool close and telemetry flush. Hooks keep today's own bounds instead: drain `ShutdownDelay` + `ShutdownTimeout` then `srv.Close`, telemetry flush 5s, relay health 2s, relay loop its 30s batch timeout. The orchestrator's grace period bounds the whole process, as before.
   - Start: `app.Start(context.WithoutCancel(ctx))`. Verified in `fx@v1.24.0/app.go` (`withRollback`): a failed start rolls back with the **same** start context, so a canceled or expired one skips every `OnStop` and leaks what was opened. Hooks that may wait long (database ping and goose lock retries, Kafka ping) derive their context with `abortOnRun`, which also cancels on `Run`'s context, so a signal during startup still aborts it promptly while rollback keeps a live context.
6. **Shutdown order through registration order.** fx runs `OnStop` in reverse `OnStart` order. `OnStart` order follows constructor execution, which follows invoke order and dependency resolution, not `fx.Provide` order. Verified in `fx@v1.24.0/module.go` (`invokeAll`): child modules' invokes run before the parent's, so telemetry is registered as `fx.Module("telemetry", fx.Invoke(startTelemetry))` first in `appOptions`; a root-level invoke would register last and flush first. Keep draining → delay → `Shutdown` → `cancelRequests`/`Close` inside **one** `OnStop` hook. For the relay, the sink hook must start before the pool hook. `TestRun_HookOrder` and `TestRunRelay_HookOrder` pin these orders.
7. **Long-lived contexts do not come from `OnStart`.** The `OnStart` context is only a start deadline. `BaseContext` for the HTTP server and the context passed to `relay.Run` come from `Run`'s `ctx` / `context.Background()` as today.
8. **Runtime errors reach the return value.** Forward `srv.Serve` and `relay.Run` errors through a channel (or `fx.Shutdowner` + `app.Wait()` with `fx.ExitCode`), and return them from `Run`/`RunRelay`.
9. **Logging.** `fx.WithLogger(func() fxevent.Logger { return &fxevent.SlogLogger{Logger: logger} })`; otherwise fx prints console text to stderr.
10. **Wiring errors are tested.** Add an `fx.ValidateApp` test for the API graph and the relay graph, because missing providers now fail at runtime instead of compile time.

## Options Considered

### Option A: fx in `internal/app` behind current signatures (chosen)
| Dimension | Assessment |
|-----------|------------|
| Complexity | Medium: lifecycle hooks and ordering rules replace sequential code and `defer` |
| Cost | New modules: fx, dig, zap (compiled in through `fxevent`); go directive 1.22, no minimum raise |
| Scalability | Graph grows by `fx.Provide`/`fx.Module`; value groups available for plug-in routes or workers |
| Team familiarity | Team standard |

**Pros:** declarative graph; per-component lifecycle; `fx.Module` reuse between API and relay; `fx.Decorate`/`fx.Replace` for tests.
**Cons:** wiring errors move from compile time to runtime; ordering is implicit; stop/start deadline semantics differ from `defer`.

### Option B: Keep manual wiring
Compile-time checked, explicit order, zero dependencies; fine for the current graph size. Rejected because the team standardizes on fx.

### Option C: `google/wire`
Compile-time DI by code generation; no lifecycle management, so the shutdown sequence stays hand-written. Rejected: adds a generator step and does not address lifecycle.

## Trade-off Analysis

The main risk is not DI but lifecycle: `defer` guarantees cleanup on every path, while fx guarantees it only for started hooks and only while the stop/start context is alive (points 4–6). Keeping the public signatures turns the existing `app_test.go` suite into the regression gate, so behavior changes surface without rewriting tests. Losing compile-time wiring checks is offset by `fx.ValidateApp` tests.

## Consequences

- Easier: adding a component is one `fx.Provide` plus a hook; API and relay can share modules (telemetry, pool).
- Harder: reasoning about order and deadlines; reading the graph requires knowing fx semantics; dig wraps constructor errors (existing `strings.Contains` assertions still match, but messages get longer).
- Supply chain: `go.uber.org/fx`, `go.uber.org/dig`, `go.uber.org/zap` join `govulncheck` scope.
- Template users (`docs/new-project.md`) must learn fx before extending the composition root.
- Revisit if the graph stays small and the ordering workarounds outweigh the benefit.

## Impact map

| File | Change |
|---|---|
| `go.mod`, `go.sum` | Add `go.uber.org/fx v1.24.0` (released 2025-05-13; `go 1.22`) |
| `internal/app/app.go` | `Run` builds an fx graph: `storageModule` (replaces `newRepository`), `newPool` (replaces `connectPostgres`), `startTelemetry` (replaces `setupTelemetry`), `newHTTPServer` with a single drain hook |
| `internal/app/relay.go` | `RunRelay` builds an fx graph: `sinkModule`/`newKafkaSink` (replace `newSink`), `newRelay`, `serveRelayHealth` hooks; sink before pool |
| `internal/adapter/outbound/postgres/user_repository.go` | Split `Connect` into `NewPool` + `Ping` |
| `internal/app/app_test.go`, `internal/app/graph_test.go` | Unchanged (regression gate); new `fx.ValidateApp` test |
| `cmd/userapi/main.go` | Unchanged |
| `.golangci.yml` | Deny `go.uber.org/fx` and `go.uber.org/dig` in `inbound-adapters`, `outbound-adapters`, `platform`, `cmd`, `everything-else` (`core` and `outbox` already allow only stdlib) |
| `docs/architecture.md` | Composition-root paragraph; depguard table; "Extending the template" steps naming `newRepository`, `newSink`, `app.Run` |
| `.claude/rules/composition-and-config.md` | Wiring names, shutdown order expressed as hook order, timeout budgets, resources opened in `OnStart` |
| `.claude/rules/messaging.md` | Relay wiring through fx |
| `AGENTS.md` | Architecture rule: fx only in `internal/app` |
| `docs/new-project.md` | Rows that point to `internal/app` wiring |
| `README.md`, `README.vi.md` | `app/` description; likely unchanged, confirm |
| `.github/workflows/ci.yml`, `Dockerfile` | No change expected |

## Action Items

1. [x] Add fx; write `fx.ValidateApp` tests first and observe them fail (TDD).
2. [x] Migrate `RunRelay`; all `TestRunRelay_*` green.
3. [x] Migrate `Run`; all `TestRun_*` green, including `TestRun_CancelsInFlightRequestsWhenDrainTimesOut` and `TestRun_DrainsBeforeClosingListener`.
4. [x] Pin start/stop hook order (`TestRun_HookOrder`, `TestRunRelay_HookOrder`): pool close and telemetry flush run after the HTTP drain.
5. [x] Add depguard denies and update the docs listed above in the same commit.
6. [x] `make check` with `TEST_DATABASE_URL` set.
7. [x] Abort startup waits on signal (`TestRun_SignalAbortsStartupWaitingOnDatabase`).
