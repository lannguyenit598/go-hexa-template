// Package domain contains entities, value objects, and business errors.
// It imports NO frameworks, drivers, or adapters:
// it is the center of the hexagonal architecture.
package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

const MaxNameLength = 100

// UserID identifies a user using canonical UUID format (36 characters, lowercase).
type UserID string

// Valid checks whether UserID has canonical UUID format.
// The service rejects malformed IDs early instead of sending them to the database.
func (id UserID) Valid() bool {
	s := string(id)
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return true
}

func (id UserID) String() string { return string(id) }

// User is an aggregate root. Use NewUser and Apply for business changes; exported fields do not enforce invariants.
type User struct {
	ID        UserID
	Email     Email
	Name      string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Version increments on each change, for optimistic locking and ETags.
	Version int64
}

// NewUser constructs a user after normalizing and validating input.
func NewUser(id UserID, email, name string, now time.Time) (User, error) {
	v := &ValidationError{}
	normEmail := collectEmail(email, v)
	normName := normalizeName(name, v)
	if v.HasErrors() {
		return User{}, v
	}
	return User{
		ID:        id,
		Email:     normEmail,
		Name:      normName,
		CreatedAt: now,
		UpdatedAt: now,
		Version:   1,
	}, nil
}

// Changes describes a partial update (PATCH): nil fields remain unchanged.
type Changes struct {
	Email *string
	Name  *string
}

// Apply applies changes to a copy of User and returns the result.
// If nothing changes, UpdatedAt and Version remain unchanged.
func (u User) Apply(c Changes, now time.Time) (User, error) {
	v := &ValidationError{}
	next := u
	if c.Email != nil {
		next.Email = collectEmail(*c.Email, v)
	}
	if c.Name != nil {
		next.Name = normalizeName(*c.Name, v)
	}
	if v.HasErrors() {
		return User{}, v
	}
	if next.Email != u.Email || next.Name != u.Name {
		next.UpdatedAt = now
		next.Version = u.Version + 1
	}
	return next, nil
}

// collectEmail parses email and collects errors in v so all invalid fields are reported together.
func collectEmail(raw string, v *ValidationError) Email {
	e, msg := parseEmail(raw)
	if msg != "" {
		v.Add("email", msg)
	}
	return e
}

func normalizeName(raw string, v *ValidationError) string {
	name := strings.Join(strings.Fields(raw), " ") // Collapse excess whitespace.
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		v.Add("name", "is required")
	case strings.ContainsRune(name, 0): // PostgreSQL TEXT cannot store U+0000.
		v.Add("name", "must not contain NUL characters")
	case n > MaxNameLength:
		v.Add("name", "must be at most 100 characters")
	}
	return name
}
