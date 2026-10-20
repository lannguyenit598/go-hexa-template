// Package service implements inbound ports by coordinating domain and outbound ports.
// It depends only on domain and port, enabling tests with in-memory adapters.
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
	// MaxOffset prevents deep pagination: OFFSET n reads and discards n rows,
	// making ?offset=50000000 expensive. Use keyset/cursor pagination for deeper traversal.
	MaxOffset = 10_000
)

type UserService struct {
	repo   port.UserRepository
	ids    port.IDGenerator
	clock  port.Clock
	logger *slog.Logger
}

// Compile-time check that UserService implements the inbound port.
var _ port.UserService = (*UserService)(nil)

// NewUserService receives a logger rather than using global slog, allowing log capture in tests
// and separate destinations per instance. A nil logger discards logs.
func NewUserService(repo port.UserRepository, ids port.IDGenerator, clock port.Clock, logger *slog.Logger) *UserService {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &UserService{repo: repo, ids: ids, clock: clock, logger: logger}
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

// parseUserID converts inbound string IDs to domain.UserID. Malformed IDs are treated as absent
// (404), keeping invalid strings out of storage (PostgreSQL would return 22P02)
// and avoiding disclosure of the ID format through validation errors.
func parseUserID(raw string) (domain.UserID, error) {
	id := domain.UserID(raw)
	if !id.Valid() {
		return "", fmt.Errorf("user %s: %w", raw, port.ErrNotFound)
	}
	return id, nil
}

func (s *UserService) Get(ctx context.Context, rawID string) (domain.User, error) {
	id, err := parseUserID(rawID)
	if err != nil {
		return domain.User{}, err
	}
	return s.get(ctx, id)
}

func (s *UserService) get(ctx context.Context, id domain.UserID) (domain.User, error) {
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

// maxUpdateAttempts limits retries on version conflicts without client If-Match.
// Apply is pure, so reading again and reapplying changes is safe.
const maxUpdateAttempts = 3

func (s *UserService) Update(ctx context.Context, cmd port.UpdateUserCmd) (domain.User, error) {
	id, err := parseUserID(cmd.ID)
	if err != nil {
		return domain.User{}, err
	}
	changes := domain.Changes{Email: cmd.Email, Name: cmd.Name}
	for attempt := 1; ; attempt++ {
		current, err := s.get(ctx, id)
		if err != nil {
			return domain.User{}, err
		}
		if cmd.ExpectedVersion != 0 && current.Version != cmd.ExpectedVersion {
			return domain.User{}, fmt.Errorf("user %s: %w", cmd.ID, port.ErrVersionConflict)
		}
		next, err := current.Apply(changes, s.clock.Now())
		if err != nil {
			return domain.User{}, err
		}
		if next == current {
			return current, nil // Nothing changed; skip the storage write.
		}
		err = s.repo.Update(ctx, next, current.Version)
		switch {
		case err == nil:
			return next, nil
		case errors.Is(err, port.ErrVersionConflict) && cmd.ExpectedVersion == 0 && attempt < maxUpdateAttempts:
			// A concurrent write occurred between read and write: read again and reapply. Log contention.
			s.logger.InfoContext(ctx, "user update hit a version conflict, retrying", "user_id", id.String(), "attempt", attempt)
			continue
		default:
			return domain.User{}, fmt.Errorf("update user: %w", err)
		}
	}
}

func (s *UserService) Delete(ctx context.Context, rawID string) error {
	id, err := parseUserID(rawID)
	if err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}
