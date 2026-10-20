// Package memory provides in-memory storage for quick development, demos, and tests.
// Data is lost when the process stops.
package memory

import (
	"cmp"
	"context"
	"slices"
	"sync"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

type UserRepository struct {
	mu      sync.RWMutex
	byID    map[domain.UserID]domain.User
	byEmail map[domain.Email]domain.UserID
}

var _ port.UserRepository = (*UserRepository)(nil)

func NewUserRepository() *UserRepository {
	return &UserRepository{
		byID:    make(map[domain.UserID]domain.User),
		byEmail: make(map[domain.Email]domain.UserID),
	}
}

func (r *UserRepository) Create(ctx context.Context, u domain.User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.byEmail[u.Email]; taken {
		return domain.ErrEmailAlreadyExists
	}
	r.byID[u.ID] = u
	r.byEmail[u.Email] = u.ID
	return nil
}

func (r *UserRepository) GetByID(ctx context.Context, id domain.UserID) (domain.User, error) {
	if err := ctx.Err(); err != nil {
		return domain.User{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	u, ok := r.byID[id]
	if !ok {
		return domain.User{}, port.ErrNotFound
	}
	return u, nil
}

func (r *UserRepository) List(ctx context.Context, limit, offset int) ([]domain.User, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	r.mu.RLock()
	all := make([]domain.User, 0, len(r.byID))
	for _, u := range r.byID {
		all = append(all, u)
	}
	r.mu.RUnlock()

	slices.SortFunc(all, func(a, b domain.User) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})

	total := len(all)
	if offset >= total {
		return []domain.User{}, total, nil
	}
	end := min(offset+limit, total)
	return all[offset:end], total, nil
}

func (r *UserRepository) Update(ctx context.Context, u domain.User, expectedVersion int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.byID[u.ID]
	if !ok {
		return port.ErrNotFound
	}
	if old.Version != expectedVersion {
		return port.ErrVersionConflict
	}
	if owner, taken := r.byEmail[u.Email]; taken && owner != u.ID {
		return domain.ErrEmailAlreadyExists
	}
	delete(r.byEmail, old.Email)
	r.byID[u.ID] = u
	r.byEmail[u.Email] = u.ID
	return nil
}

func (r *UserRepository) Delete(ctx context.Context, id domain.UserID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	u, ok := r.byID[id]
	if !ok {
		return port.ErrNotFound
	}
	delete(r.byID, id)
	delete(r.byEmail, u.Email)
	return nil
}
