package domain

import (
	"net/mail"
	"strings"
)

const MaxEmailLength = 254

// Email is a value object. NewUser and Apply validate input; use EmailFromStore for persisted
// data. The zero value is empty; callers must not assume it has been validated.
// Its private field prevents direct assignment of arbitrary strings to User.Email.
type Email struct{ v string }

// EmailFromStore reconstructs persisted data WITHOUT revalidating it. This is intentional:
// stricter future validation must not make legacy records unreadable and turn reads into 500s.
// Use only in storage adapters when restoring persisted values.
func EmailFromStore(s string) Email { return Email{v: s} }

func (e Email) String() string { return e.v }

// parseEmail normalizes (trim, lowercase) and validates an email address. It returns an empty error message for valid input.
func parseEmail(raw string) (Email, string) {
	email := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case email == "":
		return Email{}, "is required"
	case len(email) > MaxEmailLength:
		return Email{}, "must be at most 254 characters"
	}
	addr, err := mail.ParseAddress(email)
	// Accept only bare addresses ("a@b.com"), not display names ("Name <a@b.com>").
	if err != nil || addr.Address != email || addr.Name != "" {
		return Email{}, "must be a valid email address"
	}
	return Email{v: email}, ""
}
