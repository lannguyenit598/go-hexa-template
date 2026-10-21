# userapi: instructions for AI agents

User-management REST API in Go 1.26 + Echo v5 + Huma v2 (typed operations, generated OpenAPI) + PostgreSQL through pgx/v5 and sqlc (generated typed queries) with Hexagonal Architecture. References: `docs/architecture.md` (developers), `docs/api.md` (HTTP contract), `docs/events.md` (event contract), `docs/operations.md` (configuration and operations), and `docs/new-project.md` (using the template).

## Commands

| Task | Command |
|---|---|
| Required pre-commit checks | `make check`: `make sqlc-check`, gofmt, `go mod tidy -diff`, golangci-lint (including depguard), `go test -race -count=1 ./...` |
| Regenerate SQL queries | `make sqlc-generate` after changing `internal/adapter/outbound/postgres/queries/*.sql`, migrations, or `sqlc.yaml`; never edit `sqlcgen/` |
| PostgreSQL tests | Set `TEST_DATABASE_URL=postgres://...` and run `make check`. Without it, PostgreSQL tests skip; state this in results |
| Run API | `make run` (in-memory) or `make run-pg` (requires Docker) |
| Migration | `DATABASE_URL=... go run ./cmd/userapi migrate` |
| Event relay (outbox → Kafka) | `STORAGE=postgres DATABASE_URL=... EVENTS_SINK=kafka KAFKA_BROKERS=... go run ./cmd/userapi relay` |

Tools: Go >= 1.26 and golangci-lint **v2.14.0**, built with Go >= the `go.mod` version or lint refuses to run. sqlc needs no install: the Makefile pins its version and runs it with `go run`. `.github/workflows/ci.yml` runs the `make check` steps plus govulncheck and a Docker build.

## Rules

Before editing or creating a file, read every rule whose paths match it.

| Editing | Read first |
|---|---|
| `internal/core/**` | `.claude/rules/core.md` |
| `internal/adapter/inbound/**` | `.claude/rules/http-adapter.md` |
| `internal/adapter/outbound/**`, `internal/adapter/outbound/postgres/{migrations,queries}/*.sql`, `sqlc.yaml` | `.claude/rules/persistence.md` |
| `internal/outbox/**`, `internal/adapter/outbound/kafka/**`, `internal/adapter/outbound/postgres/outbox*.go`, `internal/app/relay.go`, `docs/events.md` | `.claude/rules/messaging.md` |
| `cmd/**`, `internal/app/**`, `internal/config/**`, `internal/observability/**`, `.env.example`, `docker-compose.yml`, `Dockerfile` | `.claude/rules/composition-and-config.md` |
| `**/*_test.go`, `internal/adapter/outbound/repotest/**`, `internal/adapter/outbound/postgres/pgtest/**` | `.claude/rules/tests.md` |
| `go.mod`, `go.sum`, `.golangci.yml`, `.github/**`, `Makefile`, `sqlc.yaml` | `.claude/rules/tooling-and-deps.md` |
| `**/*.md`, `api.http` | `.claude/rules/docs.md` |

## Architecture: mandatory rules

- Imports point inward: `domain` ← `port` ← `service` ← adapter ← `internal/app` ← `cmd`. `internal/core/**` imports only the standard library and itself. depguard in `.golangci.yml` enforces this; fix code, **not** lint rules, when it fails.
- Inbound ports (`port.UserService`) accept primitives (`string`, `*string`, `int`, `int64`); services convert and validate domain types. Results may use domain types.
- Errors: business rules in `domain` (`ValidationError`, `ErrEmailAlreadyExists`); shared use-case outcomes in `port` (`ErrNotFound`, `ErrVersionConflict`). Repositories return bare sentinels; services wrap context (`fmt.Errorf("get user: %w", err)`). Compare with `errors.Is`/`errors.As`. Core does not know HTTP status codes.
- No global core state: inject loggers, clocks, and ID generators through constructors/ports (`forbidigo` and `gochecknoglobals` enforce this in `internal/core`).
- Only the composition root knows adapters: `internal/app` wires them with Uber Fx (the only package importing fx); `cmd/userapi` calls only `internal/app`. Unclassified new packages use depguard `everything-else`, which forbids adapter imports.

## Conventions

- All repository content must be in English: code comments, documentation, examples, configuration guidance, and agent instructions. **Only `README.vi.md` may use Vietnamese.** Commit subjects are imperative English; bodies explain *why*.
- Do not add abstractions without a use case (for example, add Unit of Work only when needed, following `docs/architecture.md`).
- Prefer dependency releases at least 2 weeks old. Before upgrading, inspect their Go directive (`go mod download -json <module>@<version>`, then read `GoMod`): an upgrade may raise the repository minimum. Security fixes may be newer.
- Branches and PRs: **base is always `main`**, compare is the working branch. Do not push directly to `main`.

## Definition of done

1. `make check` passes; report whether PostgreSQL tests ran or skipped.
2. Observe tests for new behavior **fail before implementation** (TDD).
3. Update documentation in the same commit for changes to:
   - HTTP contracts → `docs/api.md` and `api.http`;
   - event types/schemas → `docs/events.md`;
   - environment variables → `docs/operations.md`, `.env.example`, `internal/config/config_test.go`;
   - architecture/dependency rules → `docs/architecture.md` and `.golangci.yml`;
   - README behavior/content → keep English `README.md` and Vietnamese `README.vi.md` aligned. Pure English translations may leave `README.vi.md` unchanged.
4. Verify new behavioral documentation claims (statuses, error messages, commands) by execution rather than source inspection alone.

## Skills

Skills live in `.agents/skills/`; `.claude/skills/` holds symlinks to them (on Windows, enable Developer Mode and clone with `git clone -c core.symlinks=true`).

| When | Skill |
|---|---|
| Add/change a use case or endpoint | `add-use-case` |
| Add migration or change schema | `add-migration` |
| Monitor/fix CI and address PR reviews | `steward` |
