// Package repotest provides the shared contract test suite for port.UserRepository.
// Every storage adapter (memory, postgres, ...) runs the SAME suite,
// checking that adapters can be swapped without affecting core behavior.
package repotest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
)

// Factory returns an EMPTY repository for each subtest.
type Factory func(t *testing.T) port.UserRepository

var base = time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)

func mkUser(t *testing.T, n int) domain.User {
	t.Helper()
	id := domain.UserID(fmt.Sprintf("00000000-0000-7000-8000-%012d", n))
	u, err := domain.NewUser(id, fmt.Sprintf("user%d@example.com", n), fmt.Sprintf("User %d", n), base.Add(time.Duration(n)*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func mustEmail(t *testing.T, s string) domain.Email {
	t.Helper()
	u, err := domain.NewUser("", s, "Name", base)
	if err != nil {
		t.Fatal(err)
	}
	return u.Email
}

func assertSameUser(t *testing.T, got, want domain.User) {
	t.Helper()
	if got.ID != want.ID || got.Email != want.Email || got.Name != want.Name ||
		!got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) || got.Version != want.Version {
		t.Fatalf("user mismatch:\n got  %+v\n want %+v", got, want)
	}
}

func Run(t *testing.T, newRepo Factory) {
	ctx := context.Background()

	t.Run("create then get", func(t *testing.T) {
		r := newRepo(t)
		u := mkUser(t, 1)
		if err := r.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		got, err := r.GetByID(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertSameUser(t, got, u)
	})

	t.Run("get missing returns ErrUserNotFound", func(t *testing.T) {
		r := newRepo(t)
		_, err := r.GetByID(ctx, mkUser(t, 99).ID)
		if !errors.Is(err, port.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("duplicate email on create", func(t *testing.T) {
		r := newRepo(t)
		a, b := mkUser(t, 1), mkUser(t, 2)
		b.Email = a.Email
		if err := r.Create(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := r.Create(ctx, b); !errors.Is(err, domain.ErrEmailAlreadyExists) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("list paginates in creation order with total", func(t *testing.T) {
		r := newRepo(t)
		for _, n := range []int{3, 1, 5, 2, 4} { // Insert out of order.
			if err := r.Create(ctx, mkUser(t, n)); err != nil {
				t.Fatal(err)
			}
		}
		page, total, err := r.List(ctx, 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		if total != 5 || len(page) != 2 {
			t.Fatalf("total=%d len=%d", total, len(page))
		}
		assertSameUser(t, page[0], mkUser(t, 2))
		assertSameUser(t, page[1], mkUser(t, 3))

		page, total, err = r.List(ctx, 10, 10)
		if err != nil || total != 5 || len(page) != 0 {
			t.Fatalf("offset past end: len=%d total=%d err=%v", len(page), total, err)
		}
	})

	t.Run("update changes data and frees old email", func(t *testing.T) {
		r := newRepo(t)
		u := mkUser(t, 1)
		if err := r.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		oldEmail := u.Email
		u.Email, u.Name, u.UpdatedAt, u.Version = mustEmail(t, "renamed@example.com"), "Renamed", u.UpdatedAt.Add(time.Hour), 2
		if err := r.Update(ctx, u, 1); err != nil {
			t.Fatal(err)
		}
		got, _ := r.GetByID(ctx, u.ID)
		assertSameUser(t, got, u)

		other := mkUser(t, 2)
		other.Email = oldEmail
		if err := r.Create(ctx, other); err != nil {
			t.Fatalf("old email should be reusable: %v", err)
		}
	})

	t.Run("update to taken email", func(t *testing.T) {
		r := newRepo(t)
		a, b := mkUser(t, 1), mkUser(t, 2)
		_ = r.Create(ctx, a)
		_ = r.Create(ctx, b)
		b.Email, b.Version = a.Email, 2
		if err := r.Update(ctx, b, 1); !errors.Is(err, domain.ErrEmailAlreadyExists) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("update missing", func(t *testing.T) {
		r := newRepo(t)
		if err := r.Update(ctx, mkUser(t, 1), 1); !errors.Is(err, port.ErrNotFound) {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("update with stale version is rejected and changes nothing", func(t *testing.T) {
		r := newRepo(t)
		u := mkUser(t, 1)
		if err := r.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
		first := u
		first.Name, first.Version = "First", 2
		if err := r.Update(ctx, first, 1); err != nil {
			t.Fatal(err)
		}
		// A second write based on the stale version 1 must fail without overwriting "First".
		stale := u
		stale.Email, stale.Version = mustEmail(t, "stale@example.com"), 2
		if err := r.Update(ctx, stale, 1); !errors.Is(err, port.ErrVersionConflict) {
			t.Fatalf("want ErrVersionConflict, got %v", err)
		}
		got, err := r.GetByID(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertSameUser(t, got, first)
	})

	t.Run("delete", func(t *testing.T) {
		r := newRepo(t)
		u := mkUser(t, 1)
		_ = r.Create(ctx, u)
		if err := r.Delete(ctx, u.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := r.GetByID(ctx, u.ID); !errors.Is(err, port.ErrNotFound) {
			t.Fatalf("after delete: %v", err)
		}
		if err := r.Delete(ctx, u.ID); !errors.Is(err, port.ErrNotFound) {
			t.Fatalf("second delete: %v", err)
		}
		if err := r.Create(ctx, u); err != nil {
			t.Fatalf("email should be free after delete: %v", err)
		}
	})
}
