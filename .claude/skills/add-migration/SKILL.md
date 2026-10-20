---
name: add-migration
description: Use when changing the PostgreSQL schema of this userapi repo — adding/renaming/dropping a column, table, index or constraint, or when a feature needs new persisted data.
argument-hint: "[schema change]"
---

# Add a migration

Requested schema change: $ARGUMENTS

Old and new pods share the new schema during rolling deployments, so migrations must be **backward-compatible with running code** (expand/contract). They run as the migrator role with `lock_timeout = 5s` and no `statement_timeout` limit.

## 1. Choose a strategy

| Goal | Approach | Deployments |
|---|---|---|
| Add a column | Nullable column or constant `DEFAULT` | 1 |
| Add `NOT NULL` to an existing column | Backfill; `ADD CONSTRAINT ... CHECK (col IS NOT NULL) NOT VALID`; then `VALIDATE CONSTRAINT` in a later migration | 2 |
| Rename / change type / remove a column | Add column → dual-write → backfill → read new column only → remove old column | 3+ |
| Index an existing table | `CREATE INDEX CONCURRENTLY` in a **separate file** with `-- +goose NO TRANSACTION` | 1 |
| Foreign key | `ADD CONSTRAINT ... NOT VALID`, then `VALIDATE CONSTRAINT` in a separate migration | 2 |

If multiple deployments are required, explain this to the user and implement only the first step in this PR.

## 2. Write the file

- **Do not modify executable SQL in merged migrations**; create a new file. Comment translations must preserve SQL and goose directives.
- Path: `internal/adapter/outbound/postgres/migrations/NNNN_short_description.sql`, with the next four-digit `NNNN` (list the directory).
- goose format:

```sql
-- +goose Up
-- Explain the reason and expand/contract strategy in English.
ALTER TABLE users ADD COLUMN IF NOT EXISTS phone TEXT;

-- +goose Down
ALTER TABLE users DROP COLUMN IF EXISTS phone;
```

- Files containing `CREATE INDEX CONCURRENTLY` must begin with `-- +goose NO TRANSACTION` and contain only that statement.

## 3. Update persistence code

In `internal/adapter/outbound/postgres/user_repository.go`, update `userRow`, `selectCols`, `INSERT`/`UPDATE`, and `toDomain`. New code must handle empty new columns in legacy records.

## 4. Verify (requires PostgreSQL and `TEST_DATABASE_URL`)

1. Apply all migrations to an empty schema with `TestPendingMigrations_EmptySchema`.
2. Run twice successfully (the second run is a no-op): `DATABASE_URL=$TEST_DATABASE_URL go run ./cmd/userapi migrate`.
3. Run contract tests and the full suite: `make check`.
4. If `Down` exists, test up → down → up in a temporary schema (the first goose CLI download takes about a minute):
   ```bash
   S=down_check
   psql "$TEST_DATABASE_URL" -c "DROP SCHEMA IF EXISTS $S CASCADE; CREATE SCHEMA $S"
   G="go run github.com/pressly/goose/v3/cmd/goose@v3.27.0 -dir internal/adapter/outbound/postgres/migrations postgres"
   DSN="$TEST_DATABASE_URL&search_path=$S"   # TEST_DATABASE_URL already contains '?'; append with '&'.
   $G "$DSN" up && $G "$DSN" down && $G "$DSN" up
   psql "$TEST_DATABASE_URL" -c "DROP SCHEMA $S CASCADE"
   ```
   Keep the goose CLI version aligned with `go.mod`.

## 5. Documentation

Update the expand/contract section of `docs/operations.md` with new migration examples or rules.

## Common mistakes

| Mistake | Consequence | Correct approach |
|---|---|---|
| Change a merged migration | Databases that applied the original never receive the change | New file |
| `ADD COLUMN ... NOT NULL` without `DEFAULT` | Fails on populated tables; old pods cannot insert | Nullable/`DEFAULT`; add `NOT NULL` later |
| `CREATE INDEX CONCURRENTLY` with other statements | "cannot run inside a transaction block" | Separate file + `NO TRANSACTION` |
| Regular `CREATE INDEX` on a large table | Blocks writes while building | `CONCURRENTLY` |
| Remove/rename a column with the new code deployment | Old pods fail with `column does not exist` | Expand/contract across deployments |
