package port

import "errors"

// Use-case outcome errors shared by all aggregates. These belong to the application layer,
// not domain: "not found" and "concurrent modification" are read/write outcomes,
// not business rules. Add context by wrapping, for example
// fmt.Errorf("user %s: %w", id, port.ErrNotFound); compare with errors.Is.
// Repositories return bare sentinels; services add context.
var (
	ErrNotFound = errors.New("not found")
	// ErrVersionConflict means the record changed since it was read (optimistic locking).
	ErrVersionConflict = errors.New("version conflict")
)
