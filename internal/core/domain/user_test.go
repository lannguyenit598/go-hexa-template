package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/example/userapi/internal/core/domain"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

const validID = domain.UserID("0192f3a4-5b6c-7d8e-9f01-23456789abcd")

func TestNewUser_NormalizesInput(t *testing.T) {
	u, err := domain.NewUser(validID, "  Alice@Example.COM ", "  Alice   Nguyen ", t0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.Email.String() != "alice@example.com" {
		t.Errorf("email = %q", u.Email)
	}
	if u.Name != "Alice Nguyen" {
		t.Errorf("name = %q", u.Name)
	}
	if !u.CreatedAt.Equal(t0) || !u.UpdatedAt.Equal(t0) {
		t.Errorf("timestamps not set")
	}
	if u.Version != 1 {
		t.Errorf("version = %d, want 1", u.Version)
	}
}

func TestNewUser_Validation(t *testing.T) {
	tests := []struct {
		name, email, userName string
		wantFields            []string
	}{
		{"empty both", "", "", []string{"email", "name"}},
		{"bad email", "not-an-email", "Bob", []string{"email"}},
		{"display-name form rejected", "Bob <bob@example.com>", "Bob", []string{"email"}},
		{"name too long", "bob@example.com", strings.Repeat("x", 101), []string{"name"}},
		{"unicode name at limit is ok", "bob@example.com", strings.Repeat("\u1eef", 100), nil},
		{"NUL in name", "bob@example.com", "a\x00b", []string{"name"}},
		{"name only NUL", "bob@example.com", "\x00", []string{"name"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.NewUser(validID, tc.email, tc.userName, t0)
			if tc.wantFields == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ve *domain.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			if len(ve.Fields) != len(tc.wantFields) {
				t.Fatalf("fields = %+v, want %v", ve.Fields, tc.wantFields)
			}
			for i, f := range tc.wantFields {
				if ve.Fields[i].Field != f {
					t.Errorf("field[%d] = %q, want %q", i, ve.Fields[i].Field, f)
				}
			}
		})
	}
}

func TestApply(t *testing.T) {
	u, _ := domain.NewUser(validID, "a@example.com", "A", t0)
	t1 := t0.Add(time.Hour)

	same := "A"
	got, err := u.Apply(domain.Changes{Name: &same}, t1)
	if err != nil || !got.UpdatedAt.Equal(t0) || got.Version != 1 {
		t.Fatalf("no-op change must not bump UpdatedAt/Version: %+v %v", got, err)
	}

	newName := "B"
	got, err = u.Apply(domain.Changes{Name: &newName}, t1)
	if err != nil || got.Name != "B" || !got.UpdatedAt.Equal(t1) || got.Version != 2 {
		t.Fatalf("rename failed: %+v %v", got, err)
	}
	if u.Name != "A" {
		t.Fatalf("Apply must not mutate the receiver")
	}

	bad := "nope"
	if _, err := u.Apply(domain.Changes{Email: &bad}, t1); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestUserID_Valid(t *testing.T) {
	cases := map[domain.UserID]bool{
		validID:                                true,
		"0192F3A4-5B6C-7D8E-9F01-23456789ABCD": false, // Must be lowercase.
		"not-a-uuid":                           false,
		"":                                     false,
		"0192f3a4x5b6c-7d8e-9f01-23456789abcd": false,
	}
	for id, want := range cases {
		if got := id.Valid(); got != want {
			t.Errorf("%q.Valid() = %v, want %v", id, got, want)
		}
	}
}

func TestNewUser_Email(t *testing.T) {
	u, err := domain.NewUser("id", "  Bob@Example.COM ", "Bob", time.Now())
	if err != nil || u.Email.String() != "bob@example.com" {
		t.Fatalf("NewUser email = %q, %v", u.Email, err)
	}
	for _, bad := range []string{"", "nope", "Bob <bob@example.com>", strings.Repeat("a", 250) + "@x.io"} {
		var ve *domain.ValidationError
		if _, err := domain.NewUser("id", bad, "Bob", time.Now()); !errors.As(err, &ve) || ve.Fields[0].Field != "email" {
			t.Errorf("NewUser(email %q) = %v, want email validation error", bad, err)
		}
	}
}

func TestEmailFromStore_DoesNotRevalidate(t *testing.T) {
	// Legacy data must remain readable even when it fails current validation rules.
	if got := domain.EmailFromStore("legacy-value").String(); got != "legacy-value" {
		t.Fatalf("got %q", got)
	}
}

// PostgreSQL TEXT cannot store U+0000: the domain rejects it so every adapter agrees.
func TestNULInName_IsRejected(t *testing.T) {
	const want = "must not contain NUL characters"
	for _, name := range []string{"a\x00b", "\x00"} {
		var ve *domain.ValidationError
		if _, err := domain.NewUser(validID, "bob@example.com", name, t0); !errors.As(err, &ve) || ve.Fields[0].Message != want {
			t.Errorf("NewUser(name %q) = %v, want %q", name, err, want)
		}
	}
	u, _ := domain.NewUser(validID, "a@example.com", "A", t0)
	bad := "a\x00b"
	var ve *domain.ValidationError
	if _, err := u.Apply(domain.Changes{Name: &bad}, t0.Add(time.Hour)); !errors.As(err, &ve) || ve.Fields[0].Message != want {
		t.Errorf("Apply(name %q) = %v, want %q", bad, err, want)
	}
}
