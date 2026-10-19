package postgres

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var embedded embed.FS

// migrationLockTimeout là lock_timeout cho phiên chạy migration. Một câu ALTER TABLE phải
// chờ lock ACCESS EXCLUSIVE; trong lúc chờ, MỌI query khác trên bảng xếp hàng sau nó.
// Thà migration thất bại sớm (rồi chạy lại) còn hơn làm treo toàn bộ traffic.
const migrationLockTimeout = 5 * time.Second

func migrationsFS() fs.FS {
	sub, err := fs.Sub(embedded, "migrations")
	if err != nil {
		panic(err) // chỉ xảy ra khi đường dẫn embed sai: lỗi lập trình
	}
	return sub
}

// Migrate áp dụng các migration còn thiếu bằng goose:
//   - Bảng goose_db_version ghi lại version đã chạy, mỗi file chạy đúng một lần.
//   - Advisory lock (giữ trên MỘT kết nối, do goose quản lý) đảm bảo nhiều replica hoặc
//     nhiều Job chạy cùng lúc không đè lên nhau.
//   - Mỗi file chạy trong transaction riêng, trừ file đánh dấu "-- +goose NO TRANSACTION"
//     (bắt buộc cho CREATE INDEX CONCURRENTLY).
//
// Migration dùng kết nối riêng với statement_timeout = 0 (tạo index có thể lâu) và
// lock_timeout ngắn, KHÔNG dùng chung pool của ứng dụng.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	db, err := openMigrationDB(dsn)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS(),
		goose.WithSessionLocker(locker),
		goose.WithSlog(logger),
	)
	if err != nil {
		return fmt.Errorf("create migration provider: %w", err)
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	for _, r := range results {
		logger.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "duration", r.Duration.String())
	}
	return nil
}

// PendingMigrations báo database còn migration chưa chạy hay không. Ứng dụng gọi hàm này lúc
// khởi động để dừng sớm với thông báo rõ ràng, thay vì chạy rồi lỗi "column does not exist".
//
// Hàm này chỉ đọc. goose.HasPending tự tạo bảng goose_db_version nếu chưa có, tức là chạy DDL
// bằng role của app; với role chỉ có quyền DML (khuyến nghị cho production) việc đó lỗi
// "permission denied". Vì vậy database chưa từng được migrate được nhận diện trước, bằng
// to_regclass (tôn trọng search_path), và coi là còn migration chưa chạy.
func PendingMigrations(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var hasVersionTable bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&hasVersionTable); err != nil {
		return false, fmt.Errorf("check migration table: %w", err)
	}
	if !hasVersionTable {
		return true, nil
	}

	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }() // không đóng pool, chỉ đóng wrapper
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrationsFS())
	if err != nil {
		return false, fmt.Errorf("create migration provider: %w", err)
	}
	return provider.HasPending(ctx)
}

func openMigrationDB(dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.RuntimeParams["statement_timeout"] = "0"
	cfg.RuntimeParams["lock_timeout"] = fmt.Sprint(migrationLockTimeout.Milliseconds())
	cfg.RuntimeParams["application_name"] = "userapi-migrate"
	return stdlib.OpenDB(*cfg), nil
}
