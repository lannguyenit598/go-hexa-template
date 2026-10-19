// Package port định nghĩa các "cổng" của hexagon.
//
//   - Inbound port (driving): những gì thế giới bên ngoài được phép yêu cầu ứng dụng làm.
//     Adapter HTTP gọi vào đây.
//   - Outbound port (driven): những gì ứng dụng cần từ thế giới bên ngoài.
//     Adapter database, đồng hồ, bộ sinh ID hiện thực các interface này.
//
// Interface được khai báo ở phía core (nơi dùng), không phải ở phía adapter (nơi hiện thực).
package port

import (
	"context"
	"time"

	"github.com/example/userapi/internal/core/domain"
)

// ---------- Inbound (driving) ----------

type RegisterUserCmd struct {
	Email string
	Name  string
}

type UpdateUserCmd struct {
	ID      domain.UserID
	Changes domain.Changes
}

type ListUsersQuery struct {
	Limit  int
	Offset int
}

type UserPage struct {
	Users  []domain.User
	Total  int
	Limit  int
	Offset int
}

// UserService là use case của ứng dụng.
type UserService interface {
	Register(ctx context.Context, cmd RegisterUserCmd) (domain.User, error)
	Get(ctx context.Context, id domain.UserID) (domain.User, error)
	List(ctx context.Context, q ListUsersQuery) (UserPage, error)
	Update(ctx context.Context, cmd UpdateUserCmd) (domain.User, error)
	Delete(ctx context.Context, id domain.UserID) error
}

// ---------- Outbound (driven) ----------

// UserRepository lưu trữ User. Hợp đồng lỗi (mọi adapter phải tuân theo):
//   - Create/Update trả domain.ErrEmailAlreadyExists khi trùng email.
//   - GetByID/Update/Delete trả domain.ErrUserNotFound khi không tồn tại.
//
// Hợp đồng này được kiểm bằng bộ test dùng chung ở package repotest.
type UserRepository interface {
	Create(ctx context.Context, u domain.User) error
	GetByID(ctx context.Context, id domain.UserID) (domain.User, error)
	// List trả về trang dữ liệu sắp xếp theo CreatedAt tăng dần (rồi tới ID), kèm tổng số bản ghi.
	List(ctx context.Context, limit, offset int) ([]domain.User, int, error)
	Update(ctx context.Context, u domain.User) error
	Delete(ctx context.Context, id domain.UserID) error
}

type IDGenerator interface {
	NewID() domain.UserID
}

type Clock interface {
	Now() time.Time
}
