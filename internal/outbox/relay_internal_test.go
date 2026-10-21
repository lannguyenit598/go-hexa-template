package outbox

import (
	"errors"
	"testing"
	"time"
)

func TestNextPurge(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		more bool
		err  error
		want time.Time
	}{
		{"failure retries after a minute", false, errors.New("timeout"), now.Add(time.Minute)},
		{"rows remain continues next iteration", true, nil, now},
		{"partial chunk or nothing deleted waits an hour", false, nil, now.Add(time.Hour)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextPurge(now, tc.more, tc.err); !got.Equal(tc.want) {
				t.Fatalf("nextPurge = %v, want %v", got, tc.want)
			}
		})
	}
}
