# Operations

Deployment and operations reference for userapi: configuration, Kubernetes, migrations, observability, and troubleshooting.

## Configuration

All configuration comes from environment variables. Invalid configuration exits immediately with code 1, reporting all errors on one line. This line is text on **stderr** (`ERROR invalid configuration error=...`), not JSON, because the JSON logger is created afterward. Full example: [`.env.example`](../.env.example).

| Variable | Default | Meaning | Constraints |
|---|---|---|---|
| `HTTP_ADDR` | `:8080` | Listen address | |
| `STORAGE` | `memory` | memory or postgres | Case-insensitive |
| `DATABASE_URL` | (empty) | PostgreSQL URL | Required for STORAGE=postgres and migrate |
| `LOG_LEVEL` | `info` | debug, info, warn, error | Case-insensitive; accepts slog forms such as warn+1. `debug` also logs Uber Fx wiring and lifecycle events |
| `REQUEST_TIMEOUT` | `10s` | Per-request context timeout | > 0 |
| `HTTP_IDLE_TIMEOUT` | `75s` | Idle keep-alive duration | > 0; must exceed LB idle timeout |
| `SHUTDOWN_DELAY` | `0s` | Continue serving after SIGTERM before closing listener | ≥ 0; Kubernetes: 5s–10s |
| `SHUTDOWN_TIMEOUT` | `10s` | Active-request drain period after SHUTDOWN_DELAY | ≥ 0 |
| `TRUSTED_PROXIES` | (empty) | Comma-separated LB/ingress CIDRs | Valid CIDRs |
| `READINESS_CHECK_DB` | `false` | Ping DB from /readyz (500ms timeout) | Go boolean syntax: true/false, 1/0, t/f, etc. |
| `API_DOCS_ENABLED` | `false` | Serve the OpenAPI document at /openapi.json and SwaggerUI at /docs; see [API documentation](#api-documentation) | Go boolean syntax |
| `DB_MAX_CONNS` | `10` | Maximum connections **per pod**; always overrides pool_max_conns in DATABASE_URL | ≥ 1 |
| `DB_MIN_CONNS` | `0` | Minimum idle-ready connections; 0 uses URL pool_min_conns or pgx default | 0 ≤ DB_MIN_CONNS ≤ DB_MAX_CONNS |
| `DB_MAX_CONN_LIFETIME` | `30m` | Maximum connection age, helping rebalance after failover | ≤ 0 uses pgx default |
| `DB_MAX_CONN_IDLE_TIME` | `5m` | Close long-idle connections | ≤ 0 uses pgx default |
| `DB_STATEMENT_TIMEOUT` | `5s` | PostgreSQL statement_timeout on app connections; expiry returns HTTP 503 | ≤ 0 leaves server configuration unchanged; positive values must be ≥ 1ms |
| `MIGRATE_ON_START` | `false` | Migrate on startup for development; production uses userapi migrate | |
| `EVENTS_SINK` | `none` | none (no outbox writes) or kafka; see [Event publishing](#event-publishing) | kafka requires STORAGE=postgres and KAFKA_BROKERS; case-insensitive |
| `KAFKA_BROKERS` | (empty) | Comma-separated Kafka-compatible broker addresses (Kafka, Redpanda, MSK...) | Required for EVENTS_SINK=kafka |
| `KAFKA_TOPIC` | `users` | Topic for user events | |
| `OUTBOX_POLL_INTERVAL` | `1s` | Relay wait between reads with no backlog | > 0 |
| `OUTBOX_BATCH_SIZE` | `100` | Maximum events per relay batch | 1–1000 |
| `OUTBOX_RETENTION` | `168h` | Published-row retention before relay cleanup | > 0 |
| `KAFKA_TLS` | `false` | Encrypt broker connections (TLS 1.2+, system CAs) | Boolean |
| `KAFKA_TLS_CA_FILE` | (empty) | PEM broker CA file, replacing system CAs for private certificates | Requires KAFKA_TLS=true |
| `KAFKA_SASL_MECHANISM` | (empty) | PLAIN, SCRAM-SHA-256, SCRAM-SHA-512; empty disables authentication | PLAIN requires KAFKA_TLS=true; case-insensitive |
| `KAFKA_SASL_USERNAME`, `KAFKA_SASL_PASSWORD` | (empty) | Relay SASL credentials; put the password in a Secret, not the image. The password is used verbatim: a trailing newline (e.g. a Secret created from `echo pw > file`) breaks authentication | Required when KAFKA_SASL_MECHANISM is set |
| `RELAY_HEALTH_ADDR` | `:8081` | userapi relay liveness HTTP address (GET /healthz) | |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (empty) | OTLP/HTTP endpoint. OTel starts when this **or** OTEL_EXPORTER_OTLP_TRACES_ENDPOINT / OTEL_EXPORTER_OTLP_METRICS_ENDPOINT is set, unless OTEL_SDK_DISABLED=true. A traces-only endpoint still sends metrics to the SDK default localhost:4318 | Standard OTEL_* variables are supported, including OTEL_SERVICE_NAME, OTEL_TRACES_SAMPLER, OTEL_SDK_DISABLED |

Durations use Go syntax: 500ms, 10s, 5m, 1h30m.

## Kubernetes deployment

### Shutdown without dropping requests

On SIGTERM, the app:
1. makes /readyz return 503 while **continuing to serve** during SHUTDOWN_DELAY;
2. closes the listener and waits up to SHUTDOWN_TIMEOUT for active requests;
3. closes the DB pool, flushes enabled telemetry for up to 5s, and exits with code 0.

If requests remain after SHUTDOWN_TIMEOUT, cancel them (499 in logs; clients see closed connections), close all connections, and exit with **code 1**, logging `userapi stopped with error` and `graceful shutdown: context deadline exceeded`. Adjust the timeout or investigate slow requests.

Kubernetes removes endpoints and updates kube-proxy/ingress *asynchronously* with SIGTERM. Closing immediately causes rolling-deployment 502s. Distroless lacks sleep for exec preStop; Kubernetes 1.30+ has preStop.sleep, but an in-app delay works across platforms.

Set terminationGracePeriodSeconds above SHUTDOWN_DELAY + SHUTDOWN_TIMEOUT + 5s for telemetry (for example 5s + 10s + 5s → choose 30s). A second signal, such as another Ctrl+C locally, terminates immediately.

### HTTP server timeouts

| Timeout | Value | Reason |
|---|---|---|
| ReadHeaderTimeout | 5s | Slowloris protection |
| ReadTimeout | 30s | Bound request reads |
| WriteTimeout | REQUEST_TIMEOUT + 5s | Let handlers return timeout errors before connection closure |
| IdleTimeout | HTTP_IDLE_TIMEOUT | Must **exceed** LB idle timeout (AWS ALB default 60s). Otherwise the LB can reuse a just-closed connection, causing hard-to-diagnose intermittent 502s |

### Readiness

By default, /readyz reflects only the pod's own state and does **not** ping the database. Shared-DB failures could otherwise remove *every* pod, leaving no service endpoints and producing LB 503s instead of fast application errors and recovery. Monitor DB through metrics/alerts. If needed, enable READINESS_CHECK_DB=true (500ms timeout) and use a sufficiently high probe failureThreshold.

### Connection pool sizing

The app explicitly sets DB_MAX_CONNS instead of pgx's max(4, runtime.NumCPU()) default, since container NumCPU reports node cores rather than the pod CPU limit. Size it so:

```
DB_MAX_CONNS × (maximum HPA replicas + deployment surge pods) < PostgreSQL max_connections
```

Reserve connections for migrations and administration. Use PgBouncer when replica counts grow.

### IP client

Behind a load balancer, RemoteAddr is its IP. Set TRUSTED_PROXIES to the exact LB/ingress CIDRs; only those X-Forwarded-For hops are trusted. Default private ranges are *not* trusted because other private-address cluster pods can forge headers.

<a id="x&#225;c-th&#7921;c"></a>

### Authentication

The template has **no authentication**, and GET /api/v1/users returns every user's email. Before real deployment, attach authentication/authorization middleware where internal/app/app.go constructs httpapi.Options:

```go
e := httpapi.NewServer(users, httpapi.Options{
	// ... existing options ...
	APIMiddleware: []echo.MiddlewareFunc{requireAuth}, // /api/v1 only, not probes
})

// Minimal example; replace with real JWT, OIDC, or secret-store API-key authentication.
func requireAuth(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if !validToken(c.Request().Header.Get("Authorization")) {
		return echo.ErrUnauthorized // mapped to 401 application/problem+json
		}
		return next(c)
	}
}
```

### API documentation

`API_DOCS_ENABLED=true` adds two public routes. They bypass `APIMiddleware`, so authentication attached there does not protect them:

| Route | Response |
|---|---|
| `GET /openapi.json` | OpenAPI 3.1 document generated from the registered operations (`application/openapi+json`) |
| `GET /docs` | SwaggerUI page. It loads pinned, integrity-checked assets from `unpkg.com`, so the browser needs access to that host; the page's `Content-Security-Policy` allows only those assets |

With the default `false`, both routes return 404, as do `/openapi.yaml`, `/openapi-3.0.json`, and `/schemas/...`, which this API never serves. The document uses the relative server `/` and declares no security scheme because the template enforces none; add one only together with real authentication.

## Migration

SQL migrations in internal/adapter/outbound/postgres/migrations are embedded in the binary and executed by [goose](https://github.com/pressly/goose):

```bash
DATABASE_URL='postgres://...?connect_timeout=10' userapi migrate
# Local docker compose database: make migrate (without DATABASE_URL; use make migrate DB_URL=... for another DB)
```

- goose_db_version tracks applied versions; each file runs once in its own transaction.
- A single-connection advisory lock makes concurrent migration Jobs/replicas safe.
- Migrations use a separate connection with statement_timeout=0 for long index builds and lock_timeout=5s, failing early on blocked ALTER TABLE instead of queuing all table queries behind it.
- userapi migrate sets no connection timeout. A host accepting TCP but not responding can hang the command; add connect_timeout in seconds to the URL or use Job activeDeadlineSeconds.
- Goose is the only migration tool. Query code generated by sqlc ([architecture](architecture.md#postgresql-adapter-sqlc-and-pgx)) is compiled into the binary: deployments run no generator, and sqlc never connects to the database.
- Startup only **reads** migration state, without DDL. Pending migrations, including an empty database, fail with `database has pending migrations`. MIGRATE_ON_START=true allows automatic migration first, for development only.

**Kubernetes:** run userapi migrate as a Job (Helm pre-upgrade or Argo CD PreSync) or initContainer with the **same image** as the app. Keep MIGRATE_ON_START=false. Compose models this ordering: migrate finishes before api starts. Both use the userapi superuser, however, so Compose does **not** model the role separation below.

### Database permissions

Separate schema-owning migration (DDL) and app (DML-only) roles so leaked app credentials cannot DROP TABLE. Run as superuser:

```sql
CREATE ROLE userapi_migrator LOGIN PASSWORD '...';
CREATE ROLE userapi_app      LOGIN PASSWORD '...';
CREATE DATABASE "userapi" OWNER userapi_migrator;
\connect "userapi"
-- PostgreSQL 15+ no longer grants every role CREATE on the public schema.
GRANT USAGE, CREATE ON SCHEMA public TO userapi_migrator;
GRANT USAGE ON SCHEMA public TO userapi_app;
ALTER DEFAULT PRIVILEGES FOR ROLE userapi_migrator IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO userapi_app;
```

Use **different DATABASE_URL values**: the migration Job uses userapi_migrator; the app uses userapi_app. Default privileges apply only to objects created by userapi_migrator, so always migrate under that role.

### Safe rolling-deployment migrations (expand/contract)

Old and new pods share the new schema during rollout; migrations must remain backward-compatible with running code:

| Goal | Approach |
|---|---|
| Add column | Nullable or with DEFAULT, as in 0002_add_users_version.sql |
| Rename/remove column | Add new → deploy dual-write → backfill → deploy new-only reads → remove old in a later deployment |
| Index large table | CREATE INDEX CONCURRENTLY in a separate file with -- +goose NO TRANSACTION; CONCURRENTLY cannot run in a transaction. Replacing an index: create the new one, then drop the old one CONCURRENTLY in another file (0005/0006) |
| Add NOT NULL / foreign key | ADD CONSTRAINT ... NOT VALID, then VALIDATE CONSTRAINT in a separate migration |

## Event publishing

userapi publishes user.registered / user.updated / user.deleted to Kafka through a transactional outbox. Consumer contract: [events.md](events.md).

Enable EVENTS_SINK=kafka with STORAGE=postgres and KAFKA_BROKERS, configured **identically** for API and relay.

Turn events on before the service takes its first write, or during a write freeze. During a rolling change, instances still running with EVENTS_SINK=none commit changes without outbox rows, and those events never exist; consumers cannot recover them from version gaps ([consumer guarantees](events.md#consumer-guarantees)).

Configuration per process:

- **API:** adds an outbox row atomically with each user creation, update, or deletion. It does not connect to Kafka; broker outages do not break API writes.
- **Relay:** run userapi relay as a separate Deployment using the **same image**:
  ```bash
  STORAGE=postgres DATABASE_URL=postgres://... EVENTS_SINK=kafka KAFKA_BROKERS=kafka-0:9092 userapi relay
  ```
  Startup checks migration state and broker connectivity; an unreachable broker exits with code 1 (`kafka ping: unable to dial: ...`). Runtime broker failures log WARN `outbox relay: publish failed, will retry` and retry after polling without exiting. Publishing a batch (the first send plus any individual re-sends of rejected records) is bounded to 25s; the whole batch, including marking results and committing, to 30s.
- **Liveness:** GET /healthz on RELAY_HEALTH_ADDR returns 200 `{"status":"ok"}` while the loop progresses, or 503 `{"status":"stalled"}` after roughly a minute stalled (30s batch + OUTBOX_POLL_INTERVAL + 30s slack), allowing Kubernetes restart. No readiness probe is needed because relay accepts no traffic. Example:
  ```yaml
  livenessProbe:
    httpGet: {path: /healthz, port: 8081}
    periodSeconds: 10
    failureThreshold: 3
  ```
- **Authenticated Kafka** (MSK, Confluent Cloud, Aiven, Redpanda Cloud): enable KAFKA_TLS=true and configure KAFKA_SASL_MECHANISM and credentials. MSK uses SASL/SCRAM over TLS on port 9096; IAM authentication is not implemented. Invalid credentials fail startup with kafka ping errors.
- **Replica count:** one relay handles most workloads and normally preserves per-user order. Concurrent replicas safely use FOR UPDATE SKIP LOCKED but can reorder a user's events; consumers must follow [the event contract](events.md#consumer-guarantees).
- **Shutdown:** SIGTERM finishes the active batch (up to 30s), logs `outbox relay stopped`, and exits with code 0. Set terminationGracePeriodSeconds above that, for example 45s; this is a recommendation, not a proven bound, because closing the pool and the broker client has no deadline of its own. A relay killed mid-batch resends that batch on the next run (at-least-once).
- **Topic:** the relay does not create topics. Provision cleanup.policy=delete and a retention period covering consumer recovery (for example 7 days for a weekend outage), with partitions sized for consumer parallelism. Events omit email/name; assess identifier retention under the project's own requirements.
- **Idle transactions:** a batch holds its outbox row locks in a transaction while it publishes. The relay sets idle_in_transaction_session_timeout=60s on its connections, but only when the server, role, database, and DSN leave it unset, so PostgreSQL ends the transaction of a relay that vanished mid-batch (for example, a dropped network path) and releases those rows. How quickly a dead connection is otherwise detected depends on the network and TCP keepalive settings.
- **DB permissions:** relay uses the app role, needing SELECT, UPDATE, DELETE on outbox, already granted by [default privileges](#database-permissions).
- **Cleanup:** relay purges published rows older than OUTBOX_RETENTION in chunks of 1000 rows, at most 10 chunks or 5s per turn, so cleanup never takes over publishing. While rows remain, the next turn runs on the next loop iteration; otherwise after an hour. A failed purge (WARN `outbox relay: purge failed`) is retried after 1 minute. Parked rows are retained.
- **Permanent rejections** (oversized/invalid records): retry the record alone; if still rejected, set parked_at and last_error, log ERROR `outbox relay: parked messages the broker rejected permanently` with IDs, and continue. Network, missing-topic, and permission errors are **temporary** and keep retrying. Fix the cause (such as topic max.message.bytes), then requeue:
  ```sql
  UPDATE outbox SET parked_at = NULL, last_error = NULL WHERE id = '<id from the log>';
  ```

**Required alerts:**
- age of the oldest unpublished outbox row: steady growth indicates a stopped/unreachable/overloaded relay;
- parked-row count greater than zero: consumers are missing events.

```sql
SELECT count(*) AS backlog, now() - min(created_at) AS oldest
FROM outbox WHERE published_at IS NULL AND parked_at IS NULL;

SELECT count(*) AS parked FROM outbox WHERE parked_at IS NOT NULL;
```

## Observability

**Logs.** JSON to stdout except startup configuration errors ([Configuration](#configuration)). One http_request log per non-probe request: INFO for 2xx/3xx, WARN for 4xx, ERROR with the underlying error for 5xx; latency is nanoseconds; `path` omits the query string, which can carry tokens or PII. Failed readiness probes log WARN `readiness check failed`. slog.*Context logs at every layer include request_id; trace_id/span_id appear with active spans (OTel enabled or upstream traceparent). Find request logs using the response X-Request-Id.

**Traces and metrics (OpenTelemetry).** Enable OTLP/HTTP via OTEL_EXPORTER_OTLP_ENDPOINT or signal-specific endpoints ([Configuration](#configuration)). Otherwise providers are no-ops. Export errors log INFO (`traces export: ...`) without stopping the app; final shutdown flush failure logs WARN `telemetry shutdown failed`.

| Source | Output |
|---|---|
| otelhttp wrapping Echo | Per-request spans named by route (GET /api/v1/users/:id); http.server.request.duration metrics with http.route |
| otelpgx | Query spans containing SQL but **no parameter values**, plus pool acquisition spans and pool metrics |

Probes are not traced. Upstream traceparent is accepted. Build version (`-ldflags "-X main.version=..."`) populates service.version and startup logs.

## Troubleshooting

| Symptom | Common cause | Action |
|---|---|---|
| Startup: database has pending migrations | Migration Job missing/failed, or empty database | Run userapi migrate as migrator; inspect Job logs |
| migrate: permission denied for schema public / failed to create version table | Migrator lacks CREATE on public (PostgreSQL 15+) | GRANT USAGE, CREATE ON SCHEMA public TO userapi_migrator; see [Permissions](#database-permissions) |
| migrate: canceling statement due to lock timeout | ALTER TABLE blocked for over 5s by a long transaction | Inspect pg_stat_activity and retry at lower load |
| Startup: ping database | Wrong DATABASE_URL, network failure, or DB not ready | Check URL/connectivity; Kubernetes restarts the pod |
| PostgreSQL: too many clients already | Sum of per-pod DB_MAX_CONNS exceeds max_connections | Reduce pool/replicas or add PgBouncer |
| Intermittent 502s on **every deploy** | Listener closes before LB removes the pod | Set SHUTDOWN_DELAY to 5s–10s and sufficient terminationGracePeriodSeconds |
| Intermittent 502s **at any time** | HTTP_IDLE_TIMEOUT below LB idle timeout | Set it above LB timeout |
| Many 409s and user update hit a version conflict, retrying | Concurrent updates of one user | Check aggressive retries or duplicate processing |
| Many 499 logs | Clients leave before responses, often due to short timeouts | Compare latency metrics/traces with client timeout |
| Many 503 request timed out | Request or SQL timeout (SQLSTATE 57014), often slow DB, locks, or exhausted pool | Inspect otelpgx acquisition spans, pool metrics, and pg_stat_activity locks |
| Same remote_ip on every log | TRUSTED_PROXIES unset | Configure LB/ingress CIDRs |
| Relay exits: kafka ping: unable to dial | Wrong KAFKA_BROKERS or broker not ready | Check addresses/network; Kubernetes restarts the pod |
| ERROR outbox relay: parked messages the broker rejected permanently | Broker rejects record content, often MESSAGE_TOO_LARGE | Inspect last_error, fix cause, and [requeue](#event-publishing) |
| Growing backlog and publish failed, will retry | Broker unreachable, missing topic (UNKNOWN_TOPIC_OR_PARTITION), or missing write permissions | Inspect error logs; events remain queued until recovery |
| Every pod unready during DB instability | READINESS_CHECK_DB enabled | Disable it or increase failureThreshold; see [Readiness](#readiness) |
