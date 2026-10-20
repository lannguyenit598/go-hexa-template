// Package postgres provides PostgreSQL storage through pgx/v5.
// This is the ONLY storage adapter area that knows SQL and database drivers.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

const (
	uniqueViolation = "23505"
	queryCanceled   = "57014" // statement_timeout or pg_cancel_backend
)

type UserRepository struct {
	pool   *pgxpool.Pool
	outbox bool
}

var _ port.UserRepository = (*UserRepository)(nil)

// NewUserRepository with outbox=true writes an event in the same transaction as each successful
// Create/Update/Delete (transactional outbox; see docs/events.md). The
// port.UserRepository contract stays unchanged; core knows nothing about events.
func NewUserRepository(pool *pgxpool.Pool, outbox bool) *UserRepository {
	return &UserRepository{pool: pool, outbox: outbox}
}

// PoolOptions configures the connection pool. Zero uses pgx defaults.
//
// Always set MaxConns explicitly: pgx defaults to max(4, runtime.NumCPU()), and in a
// container NumCPU reports the NODE's core count rather than the pod CPU limit.
// A 1-CPU pod on a 64-core node may open 64 connections; multiplied by replicas, this can
// exceed PostgreSQL max_connections (default 100).
type PoolOptions struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// StatementTimeout sets the server-side statement_timeout to bound queries even after
	// the client leaves (context cancellation does not always reach the server).
	StatementTimeout time.Duration
}

// Connect creates a pool and immediately checks connectivity.
func Connect(ctx context.Context, dsn string, opts PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if opts.MaxConns > 0 {
		cfg.MaxConns = opts.MaxConns
	}
	if opts.MinConns > 0 {
		cfg.MinConns = opts.MinConns
	}
	if opts.MaxConnLifetime > 0 {
		cfg.MaxConnLifetime = opts.MaxConnLifetime
	}
	if opts.MaxConnIdleTime > 0 {
		cfg.MaxConnIdleTime = opts.MaxConnIdleTime
	}
	if opts.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(opts.StatementTimeout.Milliseconds(), 10)
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "userapi"
	// Trace each query (SQL only, NEVER parameter values that could leak personal data)
	// and pool acquisition waits, exposing connection pool exhaustion in traces.
	// Global providers make this a no-op when OpenTelemetry is disabled.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer()

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	// Pool metrics (busy/idle connections, acquisition wait...): the first signals to check
	// when latency rises while database CPU remains low.
	if err := otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("record pool stats: %w", err)
	}
	return pool, nil
}

// userRow is the persistence model, separate from domain.User so the schema and domain can evolve independently.
type userRow struct {
	ID        string    `db:"id"`
	Email     string    `db:"email"`
	Name      string    `db:"name"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
	Version   int64     `db:"version"`
}

func (r userRow) toDomain() domain.User {
	return domain.User{
		ID:        domain.UserID(r.ID),
		Email:     domain.EmailFromStore(r.Email),
		Name:      r.Name,
		CreatedAt: r.CreatedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(),
		Version:   r.Version,
	}
}

const selectCols = `id::text AS id, email, name, created_at, updated_at, version`

func (r *UserRepository) Create(ctx context.Context, u domain.User) error {
	return mapError(r.write(ctx, func(q querier) error {
		if _, err := q.Exec(ctx,
			`INSERT INTO users (id, email, name, created_at, updated_at, version) VALUES ($1, $2, $3, $4, $5, $6)`,
			u.ID.String(), u.Email.String(), u.Name, u.CreatedAt, u.UpdatedAt, u.Version); err != nil {
			return err
		}
		return r.appendEvent(ctx, q, eventUserRegistered, u.ID, u.Version, u.CreatedAt)
	}))
}

func (r *UserRepository) GetByID(ctx context.Context, id domain.UserID) (domain.User, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+selectCols+` FROM users WHERE id = $1`, id.String())
	if err != nil {
		return domain.User{}, mapError(err)
	}
	row, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[userRow])
	if err != nil {
		return domain.User{}, mapError(err)
	}
	return row.toDomain(), nil
}

func (r *UserRepository) List(ctx context.Context, limit, offset int) ([]domain.User, int, error) {
	var total int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&total); err != nil {
		return nil, 0, mapError(err)
	}
	rows, err := r.pool.Query(ctx,
		`SELECT `+selectCols+` FROM users ORDER BY created_at, id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, mapError(err)
	}
	dbRows, err := pgx.CollectRows(rows, pgx.RowToStructByName[userRow])
	if err != nil {
		return nil, 0, mapError(err)
	}
	users := make([]domain.User, len(dbRows))
	for i, row := range dbRows {
		users[i] = row.toDomain()
	}
	return users, total, nil
}

// Update uses compare-and-set on version: one UPDATE (plus an outbox INSERT in the same
// transaction when enabled), without SELECT ... FOR UPDATE, so no lock is held between
// the initial read and the write.
func (r *UserRepository) Update(ctx context.Context, u domain.User, expectedVersion int64) error {
	var updated bool
	err := r.write(ctx, func(q querier) error {
		tag, err := q.Exec(ctx,
			`UPDATE users SET email = $2, name = $3, updated_at = $4, version = $5 WHERE id = $1 AND version = $6`,
			u.ID.String(), u.Email.String(), u.Name, u.UpdatedAt, u.Version, expectedVersion)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		updated = true
		return r.appendEvent(ctx, q, eventUserUpdated, u.ID, u.Version, u.UpdatedAt)
	})
	if err != nil {
		return mapError(err)
	}
	if updated {
		return nil
	}
	// No rows updated: either the record is absent or its version changed.
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, u.ID.String()).Scan(&exists); err != nil {
		return mapError(err)
	}
	if !exists {
		return port.ErrNotFound
	}
	return port.ErrVersionConflict
}

func (r *UserRepository) Delete(ctx context.Context, id domain.UserID) error {
	return mapError(r.write(ctx, func(q querier) error {
		var version int64
		if err := q.QueryRow(ctx, `DELETE FROM users WHERE id = $1 RETURNING version`, id.String()).Scan(&version); err != nil {
			return err // pgx.ErrNoRows => port.ErrNotFound qua mapError
		}
		// user.deleted uses the last version + 1: event versions strictly increase per user,
		// letting consumers discard stale versions without losing deletion events.
		return r.appendEvent(ctx, q, eventUserDeleted, id, version+1, time.Now().UTC().Truncate(time.Microsecond))
	}))
}

// mapError translates driver errors into core errors so core never sees pgx.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return port.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "users_email_key" {
		return domain.ErrEmailAlreadyExists
	}
	if errors.As(err, &pgErr) && pgErr.Code == queryCanceled {
		// An expired statement_timeout is equivalent to a request timeout: expose it as
		// context.DeadlineExceeded (HTTP 503), not an unexpected error (500).
		return fmt.Errorf("postgres: %w: %w", context.DeadlineExceeded, err)
	}
	return fmt.Errorf("postgres: %w", err)
}
