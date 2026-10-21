-- name: SetDefaultIdleInTransactionTimeout :exec
-- Applies only when nothing (server, role, database, DSN) configured the setting: source 'default'.
SELECT set_config('idle_in_transaction_session_timeout', sqlc.arg(timeout_ms)::text, false)
WHERE (SELECT source FROM pg_catalog.pg_settings WHERE name = 'idle_in_transaction_session_timeout') = 'default';
