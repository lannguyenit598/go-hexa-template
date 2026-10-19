#!/bin/bash
# SessionStart hook cho Claude Code cloud session: chuẩn bị đủ công cụ để `make check` chạy được.
#   1. Toolchain Go theo go.mod (tải tự động qua GOTOOLCHAIN=auto) và các module.
#   2. golangci-lint đúng phiên bản CI (phải build bằng Go >= dòng `go` trong go.mod).
#   3. PostgreSQL cục bộ (nếu image có sẵn) và TEST_DATABASE_URL, để chạy cả integration test.
# Idempotent: chạy lại nhiều lần an toàn. Chỉ chạy trong cloud session.
set -euo pipefail

if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel)}"

# Giữ đồng bộ với .github/workflows/ci.yml.
LINT_VERSION="v2.14.0"
PG_PORT=5433
PG_DATA=/var/lib/postgresql/claude-test

persist_env() {
  if [ -n "${CLAUDE_ENV_FILE:-}" ]; then
    echo "$1" >> "$CLAUDE_ENV_FILE"
  fi
}

# 1. Toolchain Go và module. Lệnh go đầu tiên trong repo tự tải toolchain mà go.mod yêu cầu.
go mod download
echo "session-start: $(go version)"

# 2. golangci-lint. Bản cài sẵn trong image có thể quá cũ cho Go của repo, nên cài đúng bản CI vào GOPATH/bin.
GOBIN_DIR="$(go env GOPATH)/bin"
if ! "$GOBIN_DIR/golangci-lint" version 2>/dev/null | grep -q "has version ${LINT_VERSION#v} "; then
  GOBIN="$GOBIN_DIR" go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${LINT_VERSION}"
fi
persist_env "export PATH=\"$GOBIN_DIR:\$PATH\""
echo "session-start: $("$GOBIN_DIR/golangci-lint" version)"

# 3. PostgreSQL cho integration test. Không bắt buộc: thiếu thì test PostgreSQL tự skip.
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
    || "${psql[@]}" -c "CREATE ROLE userapi LOGIN SUPERUSER PASSWORD 'userapi'" || return 1
  "${psql[@]}" -c "SELECT 1 FROM pg_database WHERE datname = 'userapi'" | grep -q 1 \
    || "${psql[@]}" -c "CREATE DATABASE userapi OWNER userapi" || return 1
}

if setup_postgres; then
  persist_env "export TEST_DATABASE_URL=postgres://userapi:userapi@localhost:${PG_PORT}/userapi?sslmode=disable"
  echo "session-start: PostgreSQL ready on port ${PG_PORT}; TEST_DATABASE_URL exported"
else
  echo "session-start: PostgreSQL not available; integration tests will be skipped" >&2
fi
