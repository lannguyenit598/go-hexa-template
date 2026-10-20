# userapi: Go + Echo v5 + Hexagonal Architecture

**English** | [Tiếng Việt](README.vi.md)

A production-ready template for a user-management REST API, organized as a Hexagonal (Ports & Adapters) architecture. It includes:
- a domain layer with input validation;
- two swappable storage adapters (in-memory and PostgreSQL) that pass the same contract test suite;
- optimistic locking with `ETag`/`If-Match`;
- RFC 9457 error responses;
- goose migrations;
- Kubernetes-friendly graceful shutdown;
- structured logging and OpenTelemetry;
- domain events published to Kafka through a transactional outbox (off by default), with the core unchanged;
- a dependency rule enforced automatically in CI.

| Component | Version |
|---|---|
| Go | 1.26 or later (the HTTP/2 security fixes in `golang.org/x/net` require Go 1.26) |
| Echo | `github.com/labstack/echo/v5` v5.4.0 |
| PostgreSQL driver | `github.com/jackc/pgx/v5` v5.11.0 |
| Migrations | `github.com/pressly/goose/v3` v3.27.0 |
| OpenTelemetry | `go.opentelemetry.io/otel` v1.46.0 |
| Kafka client | `github.com/twmb/franz-go` v1.22.1 |
| PostgreSQL | 16 |

## Documentation

The detailed documents are currently written in Vietnamese.

| If you are | Read |
|---|---|
| Calling the API | [docs/api.md](docs/api.md): endpoints, schema, headers, pagination, `ETag`/`If-Match`, full error table |
| Deploying or operating the service | [docs/operations.md](docs/operations.md): configuration, Kubernetes, migrations and database roles, observability, troubleshooting |
| Building on the template | [docs/architecture.md](docs/architecture.md): dependency rule, request flow, design decisions, how to extend |
| Consuming the service's events | [docs/events.md](docs/events.md): event types, message format, delivery and ordering guarantees |
| Starting a new project from this template | [docs/new-project.md](docs/new-project.md): renaming, removing what you don't need, GitHub setup, decisions to make before production |

`docs/superpowers/` holds the specs and plans of earlier changes. They are historical records, accurate as of when they were written (line numbers in them may be out of date); do not use them as reference documentation.

## Requirements

- Go 1.26 or later
- Docker with Compose v2 (`docker compose`), for commands that need PostgreSQL
- [golangci-lint](https://golangci-lint.run/) v2.14 or later (built with Go 1.26), for `make lint`. CI uses v2.14.0

## Quick start

```bash
# 1. No database: data lives in memory
make run                      # or: go run ./cmd/userapi

# 2. With PostgreSQL: start the DB with docker compose, run migrations, then run the API
make run-pg

# 3. The whole stack in containers (DB, migrations, API)
make up
```

Try the API:

```bash
curl -i -X POST localhost:8080/api/v1/users \
  -H 'Content-Type: application/json' \
  -d '{"email":"Lan@Example.com","name":"Lan  Nguyen"}'
# 201 Created, Location: /api/v1/users/<uuid>, ETag: "1"
# the email is lowercased and repeated spaces in the name are collapsed

curl 'localhost:8080/api/v1/users?limit=10'
```

[`api.http`](api.http) contains sample requests for REST Client (VS Code) or the JetBrains HTTP Client.

> The template has **no authentication**. Read [operations.md › Xác thực](docs/operations.md#xác-thực) (Authentication) before deploying it anywhere real.

## Architecture

The core (`domain`, `port`, `service`) does not depend on any framework or database. The inbound adapter (HTTP) calls the core through the inbound port; outbound adapters (PostgreSQL, in-memory, clock, ID generator) implement the outbound ports. Only the composition root (`cmd/userapi` + `internal/app`) knows about everything and wires it together. Diagram, details and the reasoning behind each decision: [docs/architecture.md](docs/architecture.md).

```
cmd/userapi/main.go          entry point: config, logger, signals, command dispatch (serve / migrate / relay)
internal/
  app/                       composition root: wires adapters into the core, readiness, drain on shutdown, server timeouts
  core/
    domain/                  User entity, Email value object, ValidationError, ErrEmailAlreadyExists
    port/                    inbound (UserService) and outbound (UserRepository, IDGenerator, Clock) interfaces;
                             use-case outcome errors (ErrNotFound, ErrVersionConflict)
    service/                 use cases: Register, Get, List, Update (PATCH), Delete
  adapter/
    inbound/httpapi/         Echo v5: routes, DTOs, middleware, mapping errors to problem+json
    outbound/memory/         in-memory repository (map + RWMutex)
    outbound/postgres/       pgx repository + outbox; migrations/ (embedded SQL, run by goose)
    outbound/postgres/pgtest/  integration-test helper: an empty, isolated PostgreSQL schema per test
    outbound/repotest/       contract test suite shared by every repository
    outbound/system/         UUIDv7 generator, UTC clock
    outbound/kafka/          sends outbox events to Kafka (franz-go)
  config/                    configuration from environment variables
  observability/             request-scoped slog handler, OpenTelemetry setup
  outbox/                    relay loop moving events from the outbox to a broker (Source/Sink)
docs/                        API, operations and architecture docs
.github/workflows/ci.yml     CI: lint, tests (with PostgreSQL), migrations, govulncheck, Docker build
api.http                     sample requests
```

## Development

```bash
make help               # list all commands
make test               # unit + HTTP + lifecycle tests, no database needed
make test-integration   # adds tests against a real PostgreSQL (TRUNCATEs the users and outbox tables in the docker compose DB!)
make migrate DB_URL=postgres://...   # any make target that needs PostgreSQL skips Docker when DB_URL is given
make lint               # golangci-lint, including depguard for the dependency rule
make build              # build the binary into ./bin, stamped with the git version
```

| Layer | Where | What it checks |
|---|---|---|
| Domain | `core/domain/*_test.go` | Normalization, validation, `Apply` does not mutate the original |
| Service | `core/service/*_test.go` | Use cases, invalid IDs, retries on version conflict; fake clock and IDs for deterministic results |
| Contract | `adapter/outbound/repotest` | **The same suite** runs against memory and postgres, including compare-and-set on version |
| PostgreSQL | `adapter/outbound/postgres/*_test.go` | 20 goroutines updating one user lose no writes; an empty database is detected as having pending migrations |
| HTTP | `adapter/inbound/httpapi/*_test.go` | CRUD, problem+json, ETag/If-Match, 499, client IP via trusted proxies, `X-Request-Id`, probes are neither logged nor traced |
| Lifecycle | `app/*_test.go` | Drain on shutdown, port already in use, pending migrations; an event written by the API reaches Kafka through the relay |
| Events | `outbox/*_test.go`, `adapter/outbound/{postgres,kafka}/*_test.go` | One event per successful write, atomic with it; relays never share a message; retry after a broker failure; Kafka tests use the in-memory `kfake` cluster |
| Observability | `observability/*_test.go` | `request_id`/`trace_id` are added to logs from the context |
| Config | `config/*_test.go` | Defaults and configuration errors |

Tests that need PostgreSQL run only when `TEST_DATABASE_URL` is set and are skipped otherwise. CI always sets it, backed by a PostgreSQL service container.

## Changing the module path

To start a new service from the template, follow [docs/new-project.md](docs/new-project.md): it renames the module path and the service name together. This section covers the module path only.

The repo uses the placeholder module path `github.com/example/userapi`. To switch to yours:

```bash
NEW=github.com/<you>/userapi
go mod edit -module $NEW
# .golangci.yml contains package paths in its depguard rules: if you skip it, lint
# reports false errors in the core and the rules that block wrong imports silently stop working.
grep -rl github.com/example/userapi --include='*.go' --include='.golangci.yml' . \
  | xargs sed -i "s#github.com/example/userapi#$NEW#g"
go build ./... && go test ./... && golangci-lint run ./...
```

On macOS, use `sed -i ''` instead of `sed -i`.

## License

[MIT](LICENSE)
