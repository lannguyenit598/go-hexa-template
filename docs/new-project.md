# Starting a project from the template

Checklist for a team starting a new service. Complete steps 1–3 on day one and steps 4–5 before the first production deployment.

## 1. Create a repository and rename

Create a repository from the GitHub template (**Use this template**; the owner must enable *Template repository* in Settings), clone it, then rename the module and service in one command. Example: `orders`.

```bash
scripts/rename.sh orders github.com/acme/orders
```

The script renames the module path (including depguard package paths in `.golangci.yml`), `cmd/userapi`, and every `userapi` string: binary, image, development database/role, PostgreSQL application_name, OpenTelemetry service.name, logs, documentation, and AI agent configuration (AGENTS.md, `.agents/`, `.claude/`), plus a local `.env` if you created one. Migrations stay unchanged (their comments do not affect behavior). It then deletes itself, the template-only `Template` workflow and the READMEs' "Starting a new service" sections, and runs `make check`, whose first run downloads and builds sqlc through the Go module proxy (network access, about a minute). It does not commit: review `git status`, then commit.

The name must match `^[a-z][a-z0-9_]*$`, because it also becomes a PostgreSQL role and database name and a Docker image tag. The script refuses missing tools or a dirty working tree (exit 1) and invalid arguments or a copy that is not a git clone, such as a ZIP download (exit 2), before changing anything. To change only the module path, keep the name: `scripts/rename.sh userapi github.com/acme/userapi`. It runs on macOS and Linux. If it fails partway, or `make check` fails, it prints an undo command for the clone (`git -C <clone> checkout -- . && git -C <clone> clean -fd`); a local `.env` is renamed only after `make check` passes.

If you are reading this inside a generated project, this step is already done.

Then update manually:
- `LICENSE`: owner and year.
- Title and introduction in both README.md and README.vi.md.
- KAFKA_TOPIC defaults to users; change its default in internal/config/config.go, .env.example, and docs/operations.md if the service publishes elsewhere.

## 2. Remove unused features

Each feature below can be disabled by configuration. Delete code only when certain it is unnecessary, including related tests, docs, and depguard rules in the same commit.

| Feature | Disable with | Remove entirely |
|---|---|---|
| Events (outbox, relay, Kafka) | EVENTS_SINK=none (default) | internal/outbox, internal/adapter/outbound/kafka, internal/app/relay.go, postgres/outbox*.go, the outbox flag of postgres.NewUserRepository, cmd relay branch, docs/events.md, outbox/outbound-adapters-no-broker rules and franz-go entries in .golangci.yml, .claude/rules/messaging.md and the messaging row in AGENTS.md's rules table. Retain migrations 0003/0004 if applied; they may be removed for a new database |
| In-memory repository | STORAGE=postgres everywhere | Keep it: service and HTTP tests use it |

## 3. Configure GitHub

- Protect main: require PRs and passing lint, test, vulncheck, and docker checks; prohibit force-push.
- Enable Dependabot alerts and security updates (Settings › Code security). .github/dependabot.yml covers Go modules, GitHub Actions, and Docker images with a 14-day release cooldown.
- Enable secret scanning and push protection.
- CI runs on PRs, main pushes, and weekly to catch newly disclosed vulnerabilities.

## 4. Decisions before production

The template deliberately leaves these choices to each project. Record decisions in docs/.

| Area | Current template | Decision needed |
|---|---|---|
| Authentication/authorization | None; GET /api/v1/users exposes every user's email | Choose JWT, OIDC, mTLS, etc., and integration point: [Authentication](operations.md#authentication) |
| HTTP TLS | HTTP only; assumes termination at ingress/load balancer | Confirm infrastructure and set TRUSTED_PROXIES to LB CIDRs |
| PostgreSQL TLS | Examples use sslmode=disable | Production: sslmode=verify-full, with sslrootcert for private CAs, in DATABASE_URL |
| CORS | No CORS headers; browsers on other origins cannot call the API | Add only for direct frontend access; wire Echo middleware in internal/app |
| Rate limiting | None | Gateway/ingress or per-user middleware |
| Security headers | No Strict-Transport-Security, X-Content-Type-Options, etc. | Usually configured at ingress |
| Secrets | Read from environment | Store DATABASE_URL and KAFKA_SASL_PASSWORD in a secret manager / Kubernetes Secret, never the image or repository |
| Images and releases | CI builds but does not push; base images use tags | Choose registry, tag convention (git describe), release push job, and optionally base-image digests managed by Dependabot |
| Kafka, when enabled | TLS and SASL (PLAIN, SCRAM) through KAFKA_TLS and KAFKA_SASL_* | Create topics and relay ACLs. MSK supports SASL/SCRAM over TLS here; IAM is not implemented. See [Event publishing](operations.md#event-publishing) |

## 5. Final checks

- make check passes with TEST_DATABASE_URL; PostgreSQL tests run rather than skip.
- CI passes on the first PR.
- Trial deployment follows [operations.md](operations.md): migrator-role Job, app-role API, /healthz and /readyz probes, SHUTDOWN_DELAY, terminationGracePeriodSeconds.
- If events are enabled, deploy relay with GET /healthz on RELAY_HEALTH_ADDR and the documented alerts for backlog and parked records.
