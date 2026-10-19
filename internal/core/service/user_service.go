// Package service hiện thực inbound port bằng cách điều phối domain và outbound port.
// Nó chỉ phụ thuộc vào domain và port, nên test được bằng adapter in-memory.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

const (
	DefaultPageSize = 20
	MaxPageSize     = 100
	// MaxOffset chặn phân trang quá sâu: OFFSET n buộc database đọc rồi bỏ đi n dòng,
	// nên ?offset=50000000 là một request rất đắt. Cần duyệt sâu hơn thì dùng keyset/cursor.
	MaxOffset = 10_000
)

type UserService struct {
	repo  port.UserRepository
	ids   port.IDGenerator
	clock port.Clock
}

// Kiểm tra lúc compile rằng UserService hiện thực đúng inbound port.
var _ port.UserService = (*UserService)(nil)

func NewUserService(repo port.UserRepository, ids port.IDGenerator, clock port.Clock) *UserService {
	return &UserService{repo: repo, ids: ids, clock: clock}
}

func (s *UserService) Register(ctx context.Context, cmd port.RegisterUserCmd) (domain.User, error) {
	u, err := domain.NewUser(s.ids.NewID(), cmd.Email, cmd.Name, s.clock.Now())
	if err != nil {
		return domain.User{}, err
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return domain.User{}, fmt.Errorf("register user: %w", err)
	}
	return u, nil
}

func (s *UserService) Get(ctx context.Context, id domain.UserID) (domain.User, error) {
	if !id.Valid() {
		return domain.User{}, domain.ErrUserNotFound
	}
	u, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return domain.User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (s *UserService) List(ctx context.Context, q port.ListUsersQuery) (port.UserPage, error) {
	limit, offset := q.Limit, q.Offset
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}
	if offset < 0 {
		offset = 0
	}
	if offset > MaxOffset {
		v := &domain.ValidationError{}
		v.Add("offset", fmt.Sprintf("must be at most %d", MaxOffset))
		return port.UserPage{}, v
	}
	users, total, err := s.repo.List(ctx, limit, offset)
	if err != nil {
		return port.UserPage{}, fmt.Errorf("list users: %w", err)
	}
	return port.UserPage{Users: users, Total: total, Limit: limit, Offset: offset}, nil
}

// maxUpdateAttempts: số lần thử khi xung đột version mà client không gửi If-Match.
// Apply là hàm thuần (pure), nên đọc lại rồi áp dụng lại thay đổi là an toàn.
const maxUpdateAttempts = 3

func (s *UserService) Update(ctx context.Context, cmd port.UpdateUserCmd) (domain.User, error) {
	for attempt := 1; ; attempt++ {
		current, err := s.Get(ctx, cmd.ID)
		if err != nil {
			return domain.User{}, err
		}
		if cmd.ExpectedVersion != 0 && current.Version != cmd.ExpectedVersion {
			return domain.User{}, domain.ErrVersionConflict
		}
		next, err := current.Apply(cmd.Changes, s.clock.Now())
		if err != nil {
			return domain.User{}, err
		}
		if next == current {
			return current, nil // không có gì thay đổi, khỏi ghi xuống storage
		}
		err = s.repo.Update(ctx, next, current.Version)
		switch {
		case err == nil:
			return next, nil
		case errors.Is(err, domain.ErrVersionConflict) && cmd.ExpectedVersion == 0 && attempt < maxUpdateAttempts:
			// Ai đó vừa ghi giữa lúc đọc và ghi: đọc lại và áp dụng lại. Log để thấy được mức tranh chấp.
			slog.InfoContext(ctx, "user update hit a version conflict, retrying", "user_id", cmd.ID.String(), "attempt", attempt)
			continue
		default:
			return domain.User{}, fmt.Errorf("update user: %w", err)
		}
	}
}

func (s *UserService) Delete(ctx context.Context, id domain.UserID) error {
	if !id.Valid() {
		return domain.ErrUserNotFound
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}
