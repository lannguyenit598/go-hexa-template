package system_test

import (
	"testing"
	"time"

	"github.com/example/userapi/internal/adapter/outbound/system"
)

// PostgreSQL stores timestamptz at microsecond precision: truncate the clock beforehand so
// creation responses match subsequent reads (ETags/time comparisons remain consistent).
func TestClock_IsUTCWithMicrosecondPrecision(t *testing.T) {
	for range 1000 {
		now := system.Clock{}.Now()
		if now.Location() != time.UTC {
			t.Fatalf("location = %v, want UTC", now.Location())
		}
		if now.Nanosecond()%1000 != 0 {
			t.Fatalf("%v has sub-microsecond precision", now)
		}
	}
}

func TestUUIDv7_FormatUniqueAndMonotonic(t *testing.T) {
	g := system.UUIDv7{}
	seen := make(map[string]struct{}, 10000)
	prev := ""
	for i := 0; i < 10000; i++ {
		id := g.NewID()
		if !id.Valid() {
			t.Fatalf("invalid uuid %q", id)
		}
		s := id.String()
		if s[14] != '7' {
			t.Fatalf("version nibble = %c, want 7 (%s)", s[14], s)
		}
		if v := s[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Fatalf("variant nibble = %c (%s)", v, s)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate id %s", s)
		}
		seen[s] = struct{}{}
		if s <= prev {
			t.Fatalf("ids not monotonic: %s after %s", s, prev)
		}
		prev = s
	}
}
