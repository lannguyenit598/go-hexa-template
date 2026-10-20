// Package port defines the hexagon's ports.
//
//   - Inbound (driving): operations the outside world can request from the application.
//     The HTTP adapter calls these.
//   - Outbound (driven): capabilities the application needs from the outside world.
//     Database, clock, and ID generator adapters implement these interfaces.
//
// Interfaces belong to core (the consumer), not adapters (the implementation).
package port

import (
	"context"
	"time"

	"github.com/example/userapi/internal/core/domain"
)

// ---------- Inbound (driving) ----------
//
// Input commands and queries contain only primitives, letting every transport (HTTP, gRPC,
// CLI, queue consumers) share one contract without knowing domain types. The service converts
// to domain types and validates inputs, including IDs. Results may contain domain types.

type RegisterUserCmd struct {
	Email string
	Name  string
}

type UpdateUserCmd struct {
	ID    string
	Email *string // nil = unchanged
	Name  *string // nil = unchanged
	// ExpectedVersion is the version read by the client (If-Match). 0 = no client precondition;
	// the service still uses optimistic locking on the version it reads, retrying conflicts.
	ExpectedVersion int64
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

// UserService defines the application's use cases.
type UserService interface {
	Register(ctx context.Context, cmd RegisterUserCmd) (domain.User, error)
	Get(ctx context.Context, id string) (domain.User, error)
	List(ctx context.Context, q ListUsersQuery) (UserPage, error)
	Update(ctx context.Context, cmd UpdateUserCmd) (domain.User, error)
	Delete(ctx context.Context, id string) error
}

// ---------- Outbound (driven) ----------

// UserRepository stores users. Error contract (required for every adapter):
//   - Create/Update return domain.ErrEmailAlreadyExists for duplicate emails.
//   - GetByID/Update/Delete return ErrNotFound for absent records.
//   - Update is compare-and-set: write only if the stored version equals expectedVersion;
//     otherwise return ErrVersionConflict. u.Version is the NEW version to store.
//
// The shared repotest suite verifies this contract.
type UserRepository interface {
	Create(ctx context.Context, u domain.User) error
	GetByID(ctx context.Context, id domain.UserID) (domain.User, error)
	// List returns a page ordered by CreatedAt ascending, then ID, plus the total record count.
	List(ctx context.Context, limit, offset int) ([]domain.User, int, error)
	Update(ctx context.Context, u domain.User, expectedVersion int64) error
	Delete(ctx context.Context, id domain.UserID) error
}

type IDGenerator interface {
	NewID() domain.UserID
}

type Clock interface {
	Now() time.Time
}
