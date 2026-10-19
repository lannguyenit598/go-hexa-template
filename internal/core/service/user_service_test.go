package service_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/example/userapi/internal/adapter/outbound/memory"
	"github.com/example/userapi/internal/core/domain"
	"github.com/example/userapi/internal/core/port"
	"github.com/example/userapi/internal/core/service"
)

// ---- test doubles cho outbound port ----

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type seqIDs struct{ n int }

func (g *seqIDs) NewID() domain.UserID {
	g.n++
	return domain.UserID(fmt.Sprintf("00000000-0000-7000-8000-%012d", g.n))
}

func newSUT() (*service.UserService, *fakeClock) {
	clock := &fakeClock{now: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	return service.NewUserService(memory.NewUserRepository(), &seqIDs{}, clock), clock
}

var ctx = context.Background()

func TestRegister(t *testing.T) {
	svc, clock := newSUT()
	u, err := svc.Register(ctx, port.RegisterUserCmd{Email: "A@Example.com", Name: "An"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "00000000-0000-7000-8000-000000000001" || u.Email != "a@example.com" || !u.CreatedAt.Equal(clock.now) {
		t.Fatalf("unexpected user %+v", u)
	}

	_, err = svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "Other"})
	if !errors.Is(err, domain.ErrEmailAlreadyExists) {
		t.Fatalf("want ErrEmailAlreadyExists, got %v", err)
	}

	_, err = svc.Register(ctx, port.RegisterUserCmd{Email: "bad", Name: ""})
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || len(ve.Fields) != 2 {
		t.Fatalf("want 2 validation errors, got %v", err)
	}
}

func TestGet_InvalidIDIsNotFound(t *testing.T) {
	svc, _ := newSUT()
	if _, err := svc.Get(ctx, "garbage"); !errors.Is(err, domain.ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestList_ClampsPaging(t *testing.T) {
	svc, clock := newSUT()
	for i := 0; i < 3; i++ {
		clock.Advance(time.Second)
		if _, err := svc.Register(ctx, port.RegisterUserCmd{Email: fmt.Sprintf("u%d@example.com", i), Name: "U"}); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		in                 port.ListUsersQuery
		wantLimit, wantOff int
		wantLen            int
	}{
		{port.ListUsersQuery{}, service.DefaultPageSize, 0, 3},
		{port.ListUsersQuery{Limit: 1000, Offset: -5}, service.MaxPageSize, 0, 3},
		{port.ListUsersQuery{Limit: 2, Offset: 2}, 2, 2, 1},
	}
	for _, tc := range tests {
		page, err := svc.List(ctx, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if page.Limit != tc.wantLimit || page.Offset != tc.wantOff || len(page.Users) != tc.wantLen || page.Total != 3 {
			t.Errorf("List(%+v) = limit %d offset %d len %d total %d", tc.in, page.Limit, page.Offset, len(page.Users), page.Total)
		}
	}
}

func TestList_RejectsDeepOffset(t *testing.T) {
	svc, _ := newSUT()
	_, err := svc.List(ctx, port.ListUsersQuery{Offset: service.MaxOffset + 1})
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || ve.Fields[0].Field != "offset" {
		t.Fatalf("want offset validation error, got %v", err)
	}
	if _, err := svc.List(ctx, port.ListUsersQuery{Offset: service.MaxOffset}); err != nil {
		t.Fatalf("offset at limit must be accepted: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	svc, clock := newSUT()
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	_, _ = svc.Register(ctx, port.RegisterUserCmd{Email: "b@example.com", Name: "B"})

	clock.Advance(time.Hour)
	name := "  Anh  Tuan "
	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Anh Tuan" || !got.UpdatedAt.Equal(clock.now) || !got.CreatedAt.Equal(u.CreatedAt) {
		t.Fatalf("unexpected %+v", got)
	}

	taken := "b@example.com"
	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Email: &taken}})
	if !errors.Is(err, domain.ErrEmailAlreadyExists) {
		t.Fatalf("want ErrEmailAlreadyExists, got %v", err)
	}

	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: "00000000-0000-7000-8000-999999999999", Changes: domain.Changes{Name: &name}})
	if !errors.Is(err, domain.ErrUserNotFound) {
		t.Fatalf("want ErrUserNotFound, got %v", err)
	}
}

// racingRepo mô phỏng một request khác ghi vào bản ghi NGAY GIỮA lúc service đọc và ghi.
type racingRepo struct {
	*memory.UserRepository
	races int // số lần còn chen ngang
}

func (r *racingRepo) Update(ctx context.Context, u domain.User, expected int64) error {
	if r.races > 0 {
		r.races--
		cur, _ := r.GetByID(ctx, u.ID)
		other := cur
		other.Email, other.Version = fmt.Sprintf("other%d@example.com", cur.Version), cur.Version+1
		if err := r.UserRepository.Update(ctx, other, cur.Version); err != nil {
			return err
		}
	}
	return r.UserRepository.Update(ctx, u, expected)
}

func newRacingSUT(races int) (*service.UserService, *racingRepo) {
	repo := &racingRepo{UserRepository: memory.NewUserRepository()}
	clock := &fakeClock{now: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	svc := service.NewUserService(repo, &seqIDs{}, clock)
	return svc, repo
}

func TestUpdate_ConcurrentWriteIsNotLost(t *testing.T) {
	svc, repo := newRacingSUT(0)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 1

	name := "Renamed"
	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}})
	if err != nil {
		t.Fatal(err)
	}
	// Không có optimistic locking, email do request kia ghi sẽ bị ghi đè về "a@example.com".
	if got.Name != "Renamed" || got.Email != "other1@example.com" || got.Version != 3 {
		t.Fatalf("concurrent change lost: %+v", got)
	}
}

func TestUpdate_GivesUpAfterRepeatedConflicts(t *testing.T) {
	svc, repo := newRacingSUT(0)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 100

	name := "Renamed"
	_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	if repo.races != 97 {
		t.Fatalf("expected exactly 3 attempts, got %d", 100-repo.races)
	}
}

func TestUpdate_ExpectedVersion(t *testing.T) {
	svc, repo := newRacingSUT(0)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	name := "Renamed"

	_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}, ExpectedVersion: 7})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("stale If-Match: want ErrVersionConflict, got %v", err)
	}

	// Khi client gửi version, xung đột lúc ghi KHÔNG được retry: client phải đọc lại.
	repo.races = 1
	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}, ExpectedVersion: 1})
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("race with If-Match: want ErrVersionConflict, got %v", err)
	}

	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID, Changes: domain.Changes{Name: &name}, ExpectedVersion: 2})
	if err != nil || got.Version != 3 {
		t.Fatalf("matching version: %+v %v", got, err)
	}
}

func TestDelete(t *testing.T) {
	svc, _ := newSUT()
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	if err := svc.Delete(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, u.ID); !errors.Is(err, domain.ErrUserNotFound) {
		t.Fatalf("got %v", err)
	}
}
