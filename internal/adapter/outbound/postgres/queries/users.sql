-- name: CreateUser :exec
INSERT INTO users (id, email, name, created_at, updated_at, version)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetUser :one
SELECT id, email, name, created_at, updated_at, version FROM users WHERE id = $1;

-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: ListUsers :many
-- Casts keep LIMIT/OFFSET bigint: sqlc would otherwise generate int32 parameters.
SELECT id, email, name, created_at, updated_at, version FROM users
ORDER BY created_at, id
LIMIT sqlc.arg(max_rows)::bigint OFFSET sqlc.arg(skip_rows)::bigint;

-- name: UpdateUser :execrows
-- Compare-and-set on version: zero rows means the user is missing or its version changed.
UPDATE users SET email = @email, name = @name, updated_at = @updated_at, version = @version
WHERE id = @id AND version = @expected_version;

-- name: UserExists :one
SELECT EXISTS (SELECT 1 FROM users WHERE id = $1);

-- name: DeleteUser :one
DELETE FROM users WHERE id = $1 RETURNING version;
