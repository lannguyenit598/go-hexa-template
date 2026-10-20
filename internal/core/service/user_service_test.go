package service_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	return service.NewUserService(memory.NewUserRepository(), &seqIDs{}, clock, nil), clock
}

var ctx = context.Background()

func TestRegister(t *testing.T) {
	svc, clock := newSUT()
	u, err := svc.Register(ctx, port.RegisterUserCmd{Email: "A@Example.com", Name: "An"})
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "00000000-0000-7000-8000-000000000001" || u.Email.String() != "a@example.com" || !u.CreatedAt.Equal(clock.now) {
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
	if _, err := svc.Get(ctx, "garbage"); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestGet_NotFoundCarriesContext(t *testing.T) {
	svc, _ := newSUT()
	_, err := svc.Get(ctx, "garbage")
	if !errors.Is(err, port.ErrNotFound) || !strings.Contains(err.Error(), "user garbage") {
		t.Fatalf("want wrapped port.ErrNotFound mentioning the id, got %v", err)
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
	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Anh Tuan" || !got.UpdatedAt.Equal(clock.now) || !got.CreatedAt.Equal(u.CreatedAt) {
		t.Fatalf("unexpected %+v", got)
	}

	taken := "b@example.com"
	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Email: &taken})
	if !errors.Is(err, domain.ErrEmailAlreadyExists) {
		t.Fatalf("want ErrEmailAlreadyExists, got %v", err)
	}

	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: "00000000-0000-7000-8000-999999999999", Name: &name})
	if !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("want ErrUserNotFound, got %v", err)
	}
}

// racingRepo simulates another request writing BETWEEN the service read and write.
type racingRepo struct {
	*memory.UserRepository
	races int // Remaining injected races.
}

func (r *racingRepo) Update(ctx context.Context, u domain.User, expected int64) error {
	if r.races > 0 {
		r.races--
		cur, _ := r.GetByID(ctx, u.ID)
		other := cur
		other.Email, other.Version = domain.EmailFromStore(fmt.Sprintf("other%d@example.com", cur.Version)), cur.Version+1
		if err := r.UserRepository.Update(ctx, other, cur.Version); err != nil {
			return err
		}
	}
	return r.UserRepository.Update(ctx, u, expected)
}

func newRacingSUT(logger *slog.Logger) (*service.UserService, *racingRepo) {
	repo := &racingRepo{UserRepository: memory.NewUserRepository()}
	clock := &fakeClock{now: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}
	svc := service.NewUserService(repo, &seqIDs{}, clock, logger)
	return svc, repo
}

func TestUpdate_ConcurrentWriteIsNotLost(t *testing.T) {
	svc, repo := newRacingSUT(nil)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 1

	name := "Renamed"
	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	// Without optimistic locking, the concurrent email change would be overwritten with "a@example.com".
	if got.Name != "Renamed" || got.Email.String() != "other1@example.com" || got.Version != 3 {
		t.Fatalf("concurrent change lost: %+v", got)
	}
}

func TestUpdate_GivesUpAfterRepeatedConflicts(t *testing.T) {
	svc, repo := newRacingSUT(nil)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 100

	name := "Renamed"
	_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name})
	if !errors.Is(err, port.ErrVersionConflict) {
		t.Fatalf("want ErrVersionConflict, got %v", err)
	}
	if repo.races != 97 {
		t.Fatalf("expected exactly 3 attempts, got %d", 100-repo.races)
	}
}

func TestUpdate_ExpectedVersion(t *testing.T) {
	svc, repo := newRacingSUT(nil)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	name := "Renamed"

	_, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name, ExpectedVersion: 7})
	if !errors.Is(err, port.ErrVersionConflict) {
		t.Fatalf("stale If-Match: want ErrVersionConflict, got %v", err)
	}

	// A client-supplied version disables conflict retries: the client must read again.
	repo.races = 1
	_, err = svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name, ExpectedVersion: 1})
	if !errors.Is(err, port.ErrVersionConflict) {
		t.Fatalf("race with If-Match: want ErrVersionConflict, got %v", err)
	}

	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name, ExpectedVersion: 2})
	if err != nil || got.Version != 3 {
		t.Fatalf("matching version: %+v %v", got, err)
	}
}

func TestUpdate_LogsRetriesThroughInjectedLogger(t *testing.T) {
	var buf bytes.Buffer
	svc, repo := newRacingSUT(slog.New(slog.NewJSONHandler(&buf, nil)))
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 1
	name := "Renamed"
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"msg":"user update hit a version conflict, retrying"`) ||
		!strings.Contains(buf.String(), `"attempt":1`) {
		t.Fatalf("retry not logged through injected logger: %s", buf.String())
	}
}

func TestNewUserService_NilLoggerIsSafe(t *testing.T) {
	svc, repo := newRacingSUT(nil)
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	repo.races = 1 // Force the logging path.
	name := "Renamed"
	if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String(), Name: &name}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdate_EmptyPatchIsNoOp(t *testing.T) {
	svc, _ := newSUT()
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	got, err := svc.Update(ctx, port.UpdateUserCmd{ID: u.ID.String()})
	if err != nil || got != u {
		t.Fatalf("empty patch must return the stored user unchanged: %+v %v", got, err)
	}
	if stored, _ := svc.Get(ctx, u.ID.String()); stored.Version != 1 {
		t.Fatalf("version bumped by empty patch: %d", stored.Version)
	}
}

// spyRepo counts repository method calls that accept an ID.
type spyRepo struct {
	port.UserRepository
	calls int
}

func (r *spyRepo) GetByID(ctx context.Context, id domain.UserID) (domain.User, error) {
	r.calls++
	return r.UserRepository.GetByID(ctx, id)
}

func (r *spyRepo) Update(ctx context.Context, u domain.User, expected int64) error {
	r.calls++
	return r.UserRepository.Update(ctx, u, expected)
}

func (r *spyRepo) Delete(ctx context.Context, id domain.UserID) error {
	r.calls++
	return r.UserRepository.Delete(ctx, id)
}

// Reject invalid IDs in the service before reaching the repository: PostgreSQL would return
// 22P02 (mapped to 500) for malformed input rather than "not found".
func TestInvalidIDsAreNotFoundForEveryUseCase(t *testing.T) {
	repo := &spyRepo{UserRepository: memory.NewUserRepository()}
	svc := service.NewUserService(repo, &seqIDs{}, &fakeClock{}, nil)
	t.Cleanup(func() {
		if repo.calls != 0 {
			t.Errorf("repository was called %d times with invalid IDs", repo.calls)
		}
	})
	name := "x"
	for _, id := range []string{"not-a-uuid", "", "0192F3A4-5B6C-7D8E-9F01-23456789ABCD"} {
		if _, err := svc.Get(ctx, id); !errors.Is(err, port.ErrNotFound) {
			t.Errorf("Get(%q) = %v", id, err)
		}
		if _, err := svc.Update(ctx, port.UpdateUserCmd{ID: id, Name: &name}); !errors.Is(err, port.ErrNotFound) {
			t.Errorf("Update(%q) = %v", id, err)
		}
		if err := svc.Delete(ctx, id); !errors.Is(err, port.ErrNotFound) {
			t.Errorf("Delete(%q) = %v", id, err)
		}
	}
}

func TestDelete(t *testing.T) {
	svc, _ := newSUT()
	u, _ := svc.Register(ctx, port.RegisterUserCmd{Email: "a@example.com", Name: "A"})
	if err := svc.Delete(ctx, u.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, u.ID.String()); !errors.Is(err, port.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}
