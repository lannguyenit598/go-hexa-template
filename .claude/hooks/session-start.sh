#!/bin/bash
# SessionStart hook for Claude Code cloud sessions: prepare the tools needed by `make check`.
#   1. Go toolchain from go.mod (downloaded through GOTOOLCHAIN=auto) and modules.
#   2. The CI version of golangci-lint (built with Go >= the `go` directive in go.mod).
#   3. Local PostgreSQL (when available in the image) and TEST_DATABASE_URL for integration tests.
# Idempotent: safe to run repeatedly. Runs only in cloud sessions.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"

# Keep in sync with .github/workflows/ci.yml.
LINT_VERSION="v2.14.0"
PG_PORT=5433
PG_DATA=/var/lib/postgresql/claude-test

persist_env() {
  # Avoid duplicate entries when the hook runs again (resume, clear, compact).
  if [ -n "${CLAUDE_ENV_FILE:-}" ] && ! grep -qxF -- "$1" "$CLAUDE_ENV_FILE" 2>/dev/null; then
    echo "$1" >> "$CLAUDE_ENV_FILE"
  fi
}

# 1. Go toolchain and modules. The first go command downloads the toolchain required by go.mod.
go mod download
echo "session-start: $(go version)"

# 2. golangci-lint. The preinstalled version may be too old; install the CI version in GOPATH/bin.
GOBIN_DIR="$(go env GOPATH)/bin"
if ! "$GOBIN_DIR/golangci-lint" version 2>/dev/null | grep -q "has version ${LINT_VERSION#v} "; then
  GOBIN="$GOBIN_DIR" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${LINT_VERSION}"
fi
persist_env "export PATH=\"$GOBIN_DIR:\$PATH\""
echo "session-start: $("$GOBIN_DIR/golangci-lint" version)"

# 3. PostgreSQL for integration tests. Optional: PostgreSQL tests skip when unavailable.
setup_postgres() {
  local pg_bin
  pg_bin="$(ls -d /usr/lib/postgresql/*/bin 2>/dev/null | sort -V | tail -1)" || return 1
  [ -n "$pg_bin" ] && [ "$(id -u)" = "0" ] && id postgres >/dev/null 2>&1 || return 1

  if [ ! -s "$PG_DATA/PG_VERSION" ]; then
    mkdir -p "$PG_DATA" && chown postgres "$PG_DATA" || return 1
    su postgres -c "$pg_bin/initdb -D $PG_DATA -U postgres -A trust" >/dev/null || return 1
  fi
  if ! su postgres -c "$pg_bin/pg_ctl -D $PG_DATA status" >/dev/null 2>&1; then
    su postgres -c "$pg_bin/pg_ctl -D $PG_DATA -o '-p $PG_PORT -k /tmp' -l $PG_DATA/server.log -w start" >/dev/null || return 1
  fi

  local psql=(psql -h /tmp -p "$PG_PORT" -U postgres -tAq)
  "${psql[@]}" -c "SELECT 1 FROM pg_roles WHERE rolname = 'userapi'" | grep -q 1 \
    || "${psql[@]}" -c "CREATE ROLE \"userapi\" LOGIN SUPERUSER PASSWORD 'userapi'" || return 1
  "${psql[@]}" -c "SELECT 1 FROM pg_database WHERE datname = 'userapi'" | grep -q 1 \
    || "${psql[@]}" -c "CREATE DATABASE \"userapi\" OWNER \"userapi\"" || return 1
}

if setup_postgres; then
  persist_env "export TEST_DATABASE_URL=postgres://userapi:userapi@localhost:${PG_PORT}/userapi?sslmode=disable"
  echo "session-start: PostgreSQL ready on port ${PG_PORT}; TEST_DATABASE_URL exported"
else
  echo "session-start: PostgreSQL not available; integration tests will be skipped" >&2
fi
