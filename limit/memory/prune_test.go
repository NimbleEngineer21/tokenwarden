package memory

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
)

func TestPruneKeepsOnlyLiveWindows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const team = "team-a"
		l, err := New(map[string]limit.Budget{team: {Tokens: 100, Window: time.Minute}})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		ctx := t.Context()
		reserve := func() limit.Reservation {
			t.Helper()
			r, err := l.Reserve(ctx, team, 1)
			if err != nil {
				t.Fatalf("Reserve: %v", err)
			}
			return r
		}
		windows := func() int {
			l.mu.Lock()
			defer l.mu.Unlock()
			return len(l.windows[team])
		}

		open := reserve()
		_ = reserve().Settle(ctx, 1)

		time.Sleep(time.Minute)
		second := reserve()
		if got := windows(); got != 2 {
			t.Fatalf("windows with an open reservation in the old one = %d, want 2", got)
		}

		_ = open.Settle(ctx, 1)
		_ = second.Settle(ctx, 1)
		time.Sleep(time.Minute)
		_ = reserve()
		if got := windows(); got != 1 {
			t.Fatalf("windows after all settled and two rollovers = %d, want 1", got)
		}
	})
}
