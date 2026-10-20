#!/usr/bin/env bash
# Turns a fresh clone of the template into a new service. Usage: scripts/rename.sh <name> <module>
# Renames the module path, cmd/userapi and every "userapi" string (except migrations), deletes the
# template-only files including this script, then runs make check. It does not commit.
# Exit codes: 0 renamed and checks pass; 1 dirty tree or make check failed; 2 invalid arguments.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: scripts/rename.sh <name> <module>" >&2
  exit 2
fi
NAME=$1
MODULE=$2

# The name becomes a binary, a Docker image tag and an unquoted PostgreSQL role/database name.
if ! [[ $NAME =~ ^[a-z][a-z0-9_]*$ ]]; then
  echo "invalid name: lowercase letters, digits, underscore; must start with a letter" >&2
  exit 2
fi

cd "$(git rev-parse --show-toplevel)"

if [ -n "$(git status --porcelain)" ]; then
  echo "working tree not clean; commit or stash first" >&2
  exit 1
fi

# Validates the module path before anything else changes.
go mod edit -module "$MODULE" || exit 2

# replace OLD NEW: literal replacement in the NUL-separated file list on stdin.
replace() {
  xargs -0 env OLD="$1" NEW="$2" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g'
}

# Names first, then the module path: a module path containing "userapi" is never rewritten.
git grep -lz userapi -- ':!go.mod' ':!go.sum' ':!internal/adapter/outbound/postgres/migrations' \
  ':!scripts/rename.sh' ':!.github/workflows/template.yml' | replace userapi "$NAME"
git grep -lz "github.com/example/$NAME" -- ':!go.mod' | replace "github.com/example/$NAME" "$MODULE"

mv cmd/userapi "cmd/$NAME"
# A name of a different length shifts aligned comments.
gofmt -w .
rm scripts/rename.sh .github/workflows/template.yml

echo "Renamed to $NAME ($MODULE). Running make check..."
if ! make check; then
  echo "rename applied but make check failed; inspect with git status" >&2
  exit 1
fi

cat <<EOF

Done. Still to do by hand (docs/new-project.md):
- LICENSE: owner and year
- README.md and README.vi.md: title and introduction
- KAFKA_TOPIC default if the service does not publish to "users"
Review the changes with git status, then commit.
EOF
