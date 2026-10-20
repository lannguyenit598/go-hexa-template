#!/usr/bin/env bash
# Turns a fresh clone of the template into a new service. Usage: scripts/rename.sh <name> <module>
# Renames the module path, cmd/userapi and every "userapi" string (except migrations), deletes the
# template-only files including this script, then runs make check. It does not commit.
# Exit codes: 0 renamed and checks pass; 1 missing tools, dirty tree, or a failure after changes began; 2 invalid arguments or not a git clone.
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: scripts/rename.sh <name> <module>" >&2
  exit 2
fi
NAME=$1
MODULE=$2

# The name becomes a binary, a Docker image tag and a PostgreSQL role/database name.
if ! [[ $NAME =~ ^[a-z][a-z0-9_]*$ ]]; then
  echo "invalid name: lowercase letters, digits, underscore; must start with a letter" >&2
  exit 2
fi

for tool in git go gofmt perl make; do
  if ! command -v "$tool" >/dev/null; then
    echo "missing required tool: $tool" >&2
    exit 1
  fi
done

# Work on the clone this script lives in, never on whatever repository contains the cwd.
cd "$(dirname "$0")/.."
if [ "$(git rev-parse --show-toplevel 2>/dev/null)" != "$(pwd -P)" ]; then
  echo "not a git clone of the template: $(pwd -P)" >&2
  exit 2
fi

# -print validates the module path without writing go.mod.
go mod edit -module "$MODULE" -print >/dev/null || exit 2

if [ -n "$(git status --porcelain --untracked-files=normal)" ]; then
  echo "working tree not clean; commit or stash first" >&2
  exit 1
fi

# Absolute, so the printed command undoes only this clone wherever it is pasted.
root=$(printf %q "$(pwd -P)")
UNDO="git -C $root checkout -- . && git -C $root clean -fd"
trap 'echo "rename failed partway; undo with: $UNDO" >&2; exit 1' ERR
go mod edit -module "$MODULE"

# replace OLD NEW: literal replacement in the NUL-separated file list on stdin.
replace() {
  xargs -0 env OLD="$1" NEW="$2" perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g'
}

# Names first, then the module path: a module path containing "userapi" is never rewritten.
git grep -lz userapi -- ':!go.mod' ':!go.sum' ':!internal/adapter/outbound/postgres/migrations' \
  ':!scripts/rename.sh' ':!.github/workflows/template.yml' | replace userapi "$NAME"
git grep -lz "github.com/example/$NAME" -- ':!go.mod' | replace "github.com/example/$NAME" "$MODULE"

[ "$NAME" = userapi ] || mv cmd/userapi "cmd/$NAME"
# A name of a different length shifts aligned comments.
gofmt -w .
rm scripts/rename.sh .github/workflows/template.yml
# The READMEs' "start a new service" sections describe this script, which no longer exists.
perl -0pi -e 's/^## Starting a new service\n.*?(?=^## |\z)//ms' README.md
perl -0pi -e 's/^## Bắt đầu service mới\n.*?(?=^## |\z)//ms' README.vi.md

echo "Renamed to $NAME ($MODULE). Running make check..."
if ! make check; then
  echo "rename applied but make check failed; fix the errors, or undo with: $UNDO" >&2
  exit 1
fi

# Last, because undo cannot restore it: a local .env (copied from .env.example) is gitignored.
if [ -f .env ]; then
  printf '.env\0' | replace userapi "$NAME"
fi

cat <<EOF

Done. Still to do by hand (docs/new-project.md):
- LICENSE: owner and year
- README.md and README.vi.md: title and introduction
- KAFKA_TOPIC default if the service does not publish to "users"
Review the changes with git status, then commit.
EOF
