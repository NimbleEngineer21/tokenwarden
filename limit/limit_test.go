package limit_test

import (
	"testing"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
)

func TestWindowOf(t *testing.T) {
	est := time.FixedZone("EST", -5*3600)
	tests := []struct {
		name   string
		t      time.Time
		length time.Duration
		want   time.Time
	}{
		{"inside minute", time.Date(2026, 11, 16, 10, 30, 45, 0, time.UTC), time.Minute, time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)},
		{"exactly on start", time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC), time.Minute, time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)},
		{"one ns before boundary", time.Date(2026, 11, 16, 10, 30, 59, 999999999, time.UTC), time.Minute, time.Date(2026, 11, 16, 10, 30, 0, 0, time.UTC)},
		{"day starts at UTC midnight", time.Date(2026, 11, 16, 21, 0, 0, 0, est), 24 * time.Hour, time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)},
		{"before epoch rounds down", time.Unix(-90, 0), time.Minute, time.Unix(-120, 0).UTC()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := limit.WindowOf(tt.t, tt.length)
			if !w.Start.Equal(tt.want) || w.Start.Location() != time.UTC {
				t.Errorf("Start = %v, want %v in UTC", w.Start, tt.want)
			}
			if w.Length != tt.length {
				t.Errorf("Length = %v, want %v", w.Length, tt.length)
			}
			if got := w.End(); !got.Equal(tt.want.Add(tt.length)) {
				t.Errorf("End = %v, want %v", got, tt.want.Add(tt.length))
			}
			if tt.t.Before(w.Start) || !tt.t.Before(w.End()) {
				t.Errorf("%v is not in [%v, %v)", tt.t, w.Start, w.End())
			}
		})
	}
}

func TestWindowOfPanicsOnBadLength(t *testing.T) {
	for _, length := range []time.Duration{0, -time.Second} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WindowOf(_, %v) did not panic", length)
				}
			}()
			limit.WindowOf(time.Now(), length)
		}()
	}
}
