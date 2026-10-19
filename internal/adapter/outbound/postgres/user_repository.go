// Package postgres là adapter lưu trữ dùng PostgreSQL qua pgx/v5.
// Đây là nơi DUY NHẤT trong repo biết tới SQL và driver database.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

const uniqueViolation = "23505"

type UserRepository struct {
	pool *pgxpool.Pool
}

var _ port.UserRepository = (*UserRepository)(nil)

func NewUserRepository(pool *pgxpool.Pool) *UserRepository {
	return &UserRepository{pool: pool}
}

// PoolOptions cấu hình pool kết nối. Giá trị 0 = dùng mặc định của pgx.
//
// Luôn đặt MaxConns tường minh: mặc định của pgx là max(4, runtime.NumCPU()), mà
// trong container NumCPU trả số core của NODE chứ không phải CPU limit của pod.
// Pod 1 CPU trên node 64 core sẽ mở tới 64 kết nối; nhân với số replica là đủ
// vượt max_connections (mặc định 100) của PostgreSQL.
type PoolOptions struct {
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	// StatementTimeout đặt statement_timeout phía server: chặn query chạy mãi kể cả
	// khi client đã bỏ đi (context bị huỷ không phải lúc nào cũng tới được server).
	StatementTimeout time.Duration
}

// Connect tạo pool kết nối và kiểm tra kết nối ngay.
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
	return pool, nil
}

// Migrate chạy lần lượt các file migrations/*.sql theo thứ tự tên.
// Các file viết theo kiểu idempotent nên chạy mỗi lần khởi động là an toàn.
// Dự án thật nên chuyển sang công cụ có bảng version (golang-migrate, goose, atlas...).
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := migrationFS.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", f, err)
		}
	}
	return nil
}

// userRow là "persistence model": tách khỏi domain.User để schema DB và domain tiến hoá độc lập.
type userRow struct {
	ID        string    `db:"id"`
	Email     string    `db:"email"`
	Name      string    `db:"name"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func (r userRow) toDomain() domain.User {
	return domain.User{
		ID:        domain.UserID(r.ID),
		Email:     r.Email,
		Name:      r.Name,
		CreatedAt: r.CreatedAt.UTC(),
		UpdatedAt: r.UpdatedAt.UTC(),
	}
}

const selectCols = `id::text AS id, email, name, created_at, updated_at`

func (r *UserRepository) Create(ctx context.Context, u domain.User) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, created_at, updated_at) VALUES ($1, $2, $3, $4, $5)`,
		u.ID.String(), u.Email, u.Name, u.CreatedAt, u.UpdatedAt)
	return mapError(err)
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

func (r *UserRepository) Update(ctx context.Context, u domain.User) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET email = $2, name = $3, updated_at = $4 WHERE id = $1`,
		u.ID.String(), u.Email, u.Name, u.UpdatedAt)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrUserNotFound
	}
	return nil
}

func (r *UserRepository) Delete(ctx context.Context, id domain.UserID) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id.String())
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrUserNotFound
	}
	return nil
}

// mapError dịch lỗi của driver sang lỗi domain, để core không bao giờ thấy pgx.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrUserNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "users_email_key" {
		return domain.ErrEmailAlreadyExists
	}
	return fmt.Errorf("postgres: %w", err)
}
