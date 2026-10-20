# Starting a project from the template

Checklist for a team starting a new service. Complete steps 1–3 on day one and steps 4–5 before the first production deployment.

## 1. Create a repository and rename

Create a repository from the GitHub template (**Use this template**; the owner must enable *Template repository* in Settings), clone it, then rename the module and service. Example: `orders`.

```bash
NAME=orders
NEW=github.com/acme/$NAME

# Rename module paths, including depguard package paths in .golangci.yml.
go mod edit -module $NEW
grep -rl github.com/example/userapi --include='*.go' --include='.golangci.yml' . \
  | xargs sed -i "s#github.com/example/userapi#$NEW#g"

# Rename the command directory, binary, image, development database/role, PostgreSQL application_name,
# OpenTelemetry service.name, logs, documentation, and Claude Code configuration.
# Leave migrations (their comments do not affect behavior) and historical documents unchanged.
git mv cmd/userapi cmd/$NAME
git grep -l userapi -- ':!docs/superpowers' ':!go.sum' ':!internal/adapter/outbound/postgres/migrations' \
  | xargs sed -i "s/userapi/$NAME/g"

make check
```

On macOS, use `sed -i ''` instead of `sed -i`. This procedure was previously tested with build, lint (including depguard enforcement under the new path), and tests passing; `userapi` remained only in migrations and docs/superpowers/.

Then update manually:
- `LICENSE`: owner and year.
- Title and introduction in both README.md and README.vi.md.
- KAFKA_TOPIC defaults to users; change its default in internal/config/config.go, .env.example, and docs/operations.md if the service publishes elsewhere.
- Remove docs/superpowers/: it records the template's history, not the new project's.

## 2. Remove unused features

Each feature below can be disabled by configuration. Delete code only when certain it is unnecessary, including related tests, docs, and depguard rules in the same commit.

| Feature | Disable with | Remove entirely |
|---|---|---|
| Events (outbox, relay, Kafka) | EVENTS_SINK=none (default) | internal/outbox, internal/adapter/outbound/kafka, internal/app/relay.go, postgres/outbox*.go, WithOutbox, cmd relay branch, docs/events.md, outbox/outbound-adapters-no-broker rules and franz-go entries in .golangci.yml, .claude/rules/messaging.md. Retain migrations 0003/0004 if applied; they may be removed for a new database |
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
