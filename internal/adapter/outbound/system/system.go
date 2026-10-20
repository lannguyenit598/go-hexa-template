// Package system provides adapters for system dependencies: the clock and ID generator.
// Ports make core tests fully deterministic.
package system

import (
	"time"

	"github.com/google/uuid"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// Clock returns UTC time truncated to microseconds to match PostgreSQL precision.
type Clock struct{}

var _ port.Clock = Clock{}

func (Clock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// UUIDv7 generates version 7 UUIDs (RFC 9562): a 48-bit millisecond timestamp plus randomness.
// Time-ordered IDs reduce database B-tree index fragmentation.
// Use google/uuid (widely tested, monotonic within one process) rather than maintaining
// a custom ID generator.
type UUIDv7 struct{}

var _ port.IDGenerator = UUIDv7{}

func NewUUIDv7() UUIDv7 { return UUIDv7{} }

func (UUIDv7) NewID() domain.UserID {
	// uuid.NewV7 only fails if crypto/rand fails, which Go guarantees will not happen.
	return domain.UserID(uuid.Must(uuid.NewV7()).String())
}
