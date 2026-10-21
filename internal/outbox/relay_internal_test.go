package outbox

import (
	"errors"
	"testing"
	"time"
)

func TestNextPurge(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		deleted int64
		err     error
		want    time.Time
	}{
		{"failure retries after a minute", 0, errors.New("timeout"), now.Add(time.Minute)},
		{"every chunk full continues next iteration", 10000, nil, now},
		{"partial chunk waits an hour", 999, nil, now.Add(time.Hour)},
		{"nothing deleted waits an hour", 0, nil, now.Add(time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPurge(now, tc.deleted, tc.err); !got.Equal(tc.want) {
				t.Fatalf("nextPurge = %v, want %v", got, tc.want)
			}
		})
	}
}
