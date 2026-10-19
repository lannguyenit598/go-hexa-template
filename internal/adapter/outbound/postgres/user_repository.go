// Package postgres là adapter lưu trữ dùng PostgreSQL qua pgx/v5.
// Đây là nơi DUY NHẤT trong repo biết tới SQL và driver database.
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
	cfg.ConnConfig.RuntimeParams["application_name"] = "userapi"
	// Span cho mỗi query (chỉ câu SQL, KHÔNG kèm giá trị tham số để không lộ PII vào trace)
	// và span chờ lấy kết nối từ pool: pool cạn kết nối hiện ra ngay trên trace.
	// Dùng provider toàn cục, nên là no-op khi OpenTelemetry chưa được bật.
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
	// Metric của pool (số kết nối đang dùng / rảnh, thời gian chờ acquire...): chỉ số đầu tiên
	// cần xem khi latency tăng mà CPU database vẫn thấp.
	if err := otelpgx.RecordStats(pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("record pool stats: %w", err)
	}
	return pool, nil
}

// userRow là "persistence model": tách khỏi domain.User để schema DB và domain tiến hoá độc lập.
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
	_, err := r.pool.Exec(ctx,
		`INSERT INTO users (id, email, name, created_at, updated_at, version) VALUES ($1, $2, $3, $4, $5, $6)`,
		u.ID.String(), u.Email.String(), u.Name, u.CreatedAt, u.UpdatedAt, u.Version)
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

// Update là compare-and-set trên cột version: một câu UPDATE duy nhất, không cần
// transaction hay SELECT ... FOR UPDATE, nên không giữ lock giữa lúc đọc và ghi.
func (r *UserRepository) Update(ctx context.Context, u domain.User, expectedVersion int64) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE users SET email = $2, name = $3, updated_at = $4, version = $5 WHERE id = $1 AND version = $6`,
		u.ID.String(), u.Email.String(), u.Name, u.UpdatedAt, u.Version, expectedVersion)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	// Không dòng nào được ghi: hoặc bản ghi không tồn tại, hoặc version đã đổi.
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, u.ID.String()).Scan(&exists); err != nil {
		return mapError(err)
	}
	if !exists {
		return domain.ErrUserNotFound
	}
	return domain.ErrVersionConflict
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
