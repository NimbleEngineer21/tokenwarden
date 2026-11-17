// Package limittest checks that a [limit.Limiter] keeps the contract of the
// limit package.
//
// An implementation calls [Run] from its own tests:
//
//	func TestConformance(t *testing.T) {
//		limittest.Run(t, func(t *testing.T, budgets map[string]limit.Budget, now func() time.Time) limit.Limiter {
//			t.Helper()
//			l, err := memory.New(budgets, memory.WithClock(now))
//			if err != nil {
//				t.Fatalf("New: %v", err)
//			}
//			return l
//		})
//	}
package limittest

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// Factory returns a new limiter with no usage that enforces budgets, keyed
// by team. The limiter must read the time only from now, which the suite
// moves forward to test window rollover. Factory reports its own failures
// with t.Fatal and registers its cleanup with t.Cleanup.
type Factory func(t *testing.T, budgets map[string]limit.Budget, now func() time.Time) limit.Limiter

const (
	team   = "team-a"
	window = time.Minute
)

// start is in the middle of a window, so that no test crosses a window end
// unless it moves the clock. It is far from the real time, so a limiter
// that reads time.Now instead of its clock fails the window checks.
var start = time.Date(2001, 2, 3, 4, 5, 15, 0, time.UTC)

// Run runs the conformance tests as subtests of t. Each subtest gets a new
// limiter from newLimiter.
func Run(t *testing.T, newLimiter Factory) {
	t.Helper()
	s := suite{newLimiter: newLimiter}
	t.Run("Reserve", s.testReserve)
	t.Run("ReserveCanceledContext", s.testReserveCanceledContext)
	t.Run("TeamsAreIndependent", s.testTeamsAreIndependent)
	t.Run("TopUp", s.testTopUp)
	t.Run("Settle", s.testSettle)
	t.Run("SettleHugeUsageSaturates", s.testSettleHugeUsageSaturates)
	t.Run("SettleTwiceOrNegative", s.testSettleTwiceOrNegative)
	t.Run("RolloverSettlesInStartWindow", s.testRolloverSettlesInStartWindow)
	t.Run("ConcurrentReserveNeverPassesBudget", s.testConcurrentReserveNeverPassesBudget)
	t.Run("ConcurrentTopUpOverspendBound", s.testConcurrentTopUpOverspendBound)
	t.Run("InflightCap", s.testInflightCap)
	t.Run("InflightCapSpansWindows", s.testInflightCapSpansWindows)
	t.Run("NoInflightCapAtZero", s.testNoInflightCapAtZero)
	t.Run("ConcurrentReserveNeverPassesInflightCap", s.testConcurrentReserveNeverPassesInflightCap)
}

type suite struct {
	newLimiter Factory
}

// clock is a manual time source that is safe for concurrent use.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// newTeamLimiter returns a limiter where team has tokens per window, and
// the clock that it reads.
func (s suite) newTeamLimiter(t *testing.T, tokens int64) (limit.Limiter, *clock) {
	t.Helper()
	c := &clock{t: start}
	return s.newLimiter(t, map[string]limit.Budget{team: {Tokens: tokens, Window: window}}, c.now), c
}

func mustReserve(t *testing.T, l limit.Limiter, tokens int64) limit.Reservation {
	t.Helper()
	r, err := l.Reserve(t.Context(), team, tokens)
	if err != nil {
		t.Fatalf("Reserve(%d): %v", tokens, err)
	}
	return r
}

// assertRemaining checks that exactly want tokens are left in team's
// current window: want tokens fit, and one more does not. It releases what
// it took.
func assertRemaining(t *testing.T, l limit.Limiter, want int64) {
	t.Helper()
	assertTeamRemaining(t, l, team, want)
}

func assertTeamRemaining(t *testing.T, l limit.Limiter, team string, want int64) {
	t.Helper()
	ctx := t.Context()
	if want > 0 {
		r, err := l.Reserve(ctx, team, want)
		if err != nil {
			t.Fatalf("team %q: Reserve(%d) with %d left: %v", team, want, want, err)
		}
		defer func() {
			if err := r.Settle(ctx, 0); err != nil {
				t.Errorf("team %q: Settle(0): %v", team, err)
			}
		}()
	}
	if r, err := l.Reserve(ctx, team, 1); !errors.Is(err, limit.ErrBudgetExceeded) {
		if err == nil {
			_ = r.Settle(ctx, 0)
		}
		t.Fatalf("team %q: Reserve(1) beyond %d left: err = %v, want ErrBudgetExceeded", team, want, err)
	}
}

// sameWindow reports whether a and b start at the same instant and have the
// same length. It does not use ==, which also compares the time zone and
// the monotonic clock reading: a limiter that reads the start back from a
// database can return the same instant in another time zone.
func sameWindow(a, b limit.Window) bool {
	return a.Start.Equal(b.Start) && a.Length == b.Length
}

// errAny marks an expected error that must match no sentinel of the limit
// package, such as an argument error.
var errAny = errors.New("any error that is no sentinel")

func checkErr(t *testing.T, err, want error) {
	t.Helper()
	switch {
	case want == nil && err != nil:
		t.Fatalf("err = %v, want nil", err)
	case want == errAny: //nolint:errorlint // errAny is a test marker, never wrapped.
		if err == nil {
			t.Fatal("err = nil, want an error")
		}
		for _, s := range []error{limit.ErrBudgetExceeded, limit.ErrNoBudget, limit.ErrReservationClosed} {
			if errors.Is(err, s) {
				t.Fatalf("err = %v, must not match %v", err, s)
			}
		}
	case want != nil && !errors.Is(err, want):
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// checkExceeded fails unless err wraps an [*limit.ExceededError] that names
// team, tokens, and window w.
func checkExceeded(t *testing.T, err error, team string, tokens int64, w limit.Window) {
	t.Helper()
	var e *limit.ExceededError
	switch {
	case !errors.As(err, &e):
		t.Fatalf("err = %v, want an *limit.ExceededError", err)
	case !errors.Is(err, limit.ErrBudgetExceeded):
		t.Fatalf("err = %v does not match ErrBudgetExceeded", err)
	case e.Team != team || e.Tokens != tokens || !sameWindow(e.Window, w):
		t.Fatalf("ExceededError = {%q %d %v}, want {%q %d %v}", e.Team, e.Tokens, e.Window, team, tokens, w)
	}
}

func (s suite) testReserve(t *testing.T) {
	tests := []struct {
		name    string
		held    int64
		team    string
		tokens  int64
		wantErr error // nil: success; errAny: an error that is no sentinel
	}{
		{"below budget", 0, team, 50, nil},
		{"exactly at budget", 60, team, 40, nil},
		{"one over budget", 60, team, 41, limit.ErrBudgetExceeded},
		{"larger than budget", 0, team, 101, limit.ErrBudgetExceeded},
		{"max int64 cannot overflow", 60, team, math.MaxInt64, limit.ErrBudgetExceeded},
		{"unknown team", 0, "team-x", 1, limit.ErrNoBudget},
		{"zero tokens", 0, team, 0, errAny},
		{"negative tokens", 0, team, -5, errAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, c := s.newTeamLimiter(t, 100)
			if tt.held > 0 {
				mustReserve(t, l, tt.held)
			}
			r, err := l.Reserve(t.Context(), tt.team, tt.tokens)
			checkErr(t, err, tt.wantErr)
			if errors.Is(tt.wantErr, limit.ErrBudgetExceeded) {
				checkExceeded(t, err, tt.team, tt.tokens, limit.WindowOf(c.now(), window))
			}
			if err != nil {
				return
			}
			if got := r.Reserved(); got != tt.tokens {
				t.Errorf("Reserved() = %d, want %d", got, tt.tokens)
			}
			if got, want := r.Window(), limit.WindowOf(c.now(), window); !sameWindow(got, want) {
				t.Errorf("Window() = %v, want %v", got, want)
			}
		})
	}
}

func (s suite) testReserveCanceledContext(t *testing.T) {
	l, _ := s.newTeamLimiter(t, 100)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Reserve(ctx, team, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertRemaining(t, l, 100)
}

func (s suite) testTeamsAreIndependent(t *testing.T) {
	const other = "team-b"
	c := &clock{t: start}
	l := s.newLimiter(t, map[string]limit.Budget{
		team:  {Tokens: 100, Window: window},
		other: {Tokens: 30, Window: window},
	}, c.now)
	ctx := t.Context()

	r := mustReserve(t, l, 100)
	assertTeamRemaining(t, l, other, 30)
	if err := r.Settle(ctx, 100); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	assertTeamRemaining(t, l, other, 30)
	assertRemaining(t, l, 0)
}

func (s suite) testTopUp(t *testing.T) {
	l, _ := s.newTeamLimiter(t, 100)
	ctx := t.Context()
	r := mustReserve(t, l, 60)

	if err := r.TopUp(ctx, 30); err != nil {
		t.Fatalf("TopUp(30): %v", err)
	}
	if got := r.Reserved(); got != 90 {
		t.Fatalf("Reserved() = %d, want 90", got)
	}
	checkExceeded(t, r.TopUp(ctx, 11), team, 11, r.Window())
	if got := r.Reserved(); got != 90 {
		t.Fatalf("Reserved() after failed TopUp = %d, want 90", got)
	}
	checkErr(t, r.TopUp(ctx, 0), errAny)
	checkErr(t, r.TopUp(ctx, math.MaxInt64), limit.ErrBudgetExceeded)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.TopUp(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("TopUp with canceled ctx: err = %v, want context.Canceled", err)
	}
	assertRemaining(t, l, 10)

	if err := r.Settle(ctx, 90); err != nil {
		t.Fatalf("Settle after failed TopUp: %v", err)
	}
	checkErr(t, r.TopUp(ctx, 1), limit.ErrReservationClosed)
	assertRemaining(t, l, 10)
}

func (s suite) testSettle(t *testing.T) {
	tests := []struct {
		name          string
		reserve, used int64
		wantRemaining int64
	}{
		{"used below reserved frees the rest", 60, 20, 80},
		{"used equals reserved", 60, 60, 40},
		{"used above reserved charges the overage", 60, 75, 25},
		{"used above budget blocks the window", 60, 130, 0},
		{"zero used releases all", 60, 0, 100},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, _ := s.newTeamLimiter(t, 100)
			r := mustReserve(t, l, tt.reserve)
			if err := r.Settle(t.Context(), tt.used); err != nil {
				t.Fatalf("Settle: %v", err)
			}
			if got := r.Reserved(); got != 0 {
				t.Errorf("Reserved() after Settle = %d, want 0", got)
			}
			assertRemaining(t, l, tt.wantRemaining)
		})
	}
}

func (s suite) testSettleHugeUsageSaturates(t *testing.T) {
	l, _ := s.newTeamLimiter(t, 100)
	rs := []limit.Reservation{mustReserve(t, l, 1), mustReserve(t, l, 1)}
	for _, r := range rs {
		if err := r.Settle(t.Context(), math.MaxInt64); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}
	assertRemaining(t, l, 0)
}

func (s suite) testSettleTwiceOrNegative(t *testing.T) {
	l, _ := s.newTeamLimiter(t, 100)
	ctx := t.Context()
	r := mustReserve(t, l, 60)

	checkErr(t, r.Settle(ctx, -1), errAny)
	assertRemaining(t, l, 40)
	if err := r.Settle(ctx, 10); err != nil {
		t.Fatalf("Settle after rejected Settle: %v", err)
	}
	checkErr(t, r.Settle(ctx, 10), limit.ErrReservationClosed)
	assertRemaining(t, l, 90)
}

func (s suite) testRolloverSettlesInStartWindow(t *testing.T) {
	l, c := s.newTeamLimiter(t, 100)
	ctx := t.Context()
	r := mustReserve(t, l, 70)
	first := limit.WindowOf(start, window)
	if got := r.Window(); !sameWindow(got, first) {
		t.Fatalf("Window() = %v, want %v", got, first)
	}

	c.advance(window)
	assertRemaining(t, l, 100)

	if err := r.TopUp(ctx, 30); err != nil {
		t.Fatalf("TopUp in the old window: %v", err)
	}
	checkErr(t, r.TopUp(ctx, 1), limit.ErrBudgetExceeded)
	if err := r.Settle(ctx, 100); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if got := r.Window(); !sameWindow(got, first) {
		t.Errorf("Window() after rollover = %v, want %v", got, first)
	}
	assertRemaining(t, l, 100)
}

func (s suite) testConcurrentReserveNeverPassesBudget(t *testing.T) {
	const budget, callers = 50, 200
	l, _ := s.newTeamLimiter(t, budget)
	ctx := t.Context()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		granted  []limit.Reservation
		badError error
	)
	for range callers {
		wg.Go(func() {
			r, err := l.Reserve(ctx, team, 1)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				granted = append(granted, r)
			case !errors.Is(err, limit.ErrBudgetExceeded):
				badError = err
			}
		})
	}
	wg.Wait()
	if badError != nil {
		t.Fatalf("unexpected error: %v", badError)
	}
	if len(granted) != budget {
		t.Fatalf("%d reservations granted, want %d", len(granted), budget)
	}
	for _, r := range granted {
		wg.Go(func() {
			if err := r.Settle(ctx, 0); err != nil {
				t.Errorf("Settle: %v", err)
			}
		})
	}
	wg.Wait()
	assertRemaining(t, l, budget)
}

// testConcurrentTopUpOverspendBound checks the bound from the design: held
// tokens never pass the budget, and the settled total passes it by at most
// the sum of each stream's usage beyond what it held.
func (s suite) testConcurrentTopUpOverspendBound(t *testing.T) {
	const budget, streams, chunk, topUps = 1000, 20, 10, 5
	l, _ := s.newTeamLimiter(t, budget)
	ctx := t.Context()

	var (
		wg                  sync.WaitGroup
		mu                  sync.Mutex
		held, used, overage int64
		errs                []error
	)
	addErr := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	for i := range streams {
		wg.Go(func() {
			r, err := l.Reserve(ctx, team, chunk)
			if err != nil {
				if !errors.Is(err, limit.ErrBudgetExceeded) {
					addErr(err)
				}
				return
			}
			for range topUps {
				if err := r.TopUp(ctx, chunk); err != nil {
					if !errors.Is(err, limit.ErrBudgetExceeded) {
						addErr(err)
					}
					break
				}
			}
			h := r.Reserved()
			extra := int64(i % 3)
			if err := r.Settle(ctx, h+extra); err != nil {
				addErr(err)
				return
			}
			mu.Lock()
			held += h
			used += h + extra
			overage += extra
			mu.Unlock()
		})
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("stream error: %v", err)
	}
	if held > budget {
		t.Errorf("held %d tokens, budget is %d", held, budget)
	}
	if used > budget+overage {
		t.Errorf("settled %d tokens, bound is %d", used, budget+overage)
	}
	assertRemaining(t, l, max(0, budget-used))
}

// newCapLimiter returns a limiter where team has 1000 tokens per window
// and at most maxInflight open reservations, and the clock that it reads.
func (s suite) newCapLimiter(t *testing.T, maxInflight int64) (limit.Limiter, *clock) {
	t.Helper()
	c := &clock{t: start}
	return s.newLimiter(t, map[string]limit.Budget{
		team:     {Tokens: 1000, Window: window, MaxInflight: maxInflight},
		"team-b": {Tokens: 1000, Window: window, MaxInflight: 1},
	}, c.now), c
}

func assertConcurrencyError(t *testing.T, err error, wantLimit int64) {
	t.Helper()
	var ce *limit.ConcurrencyError
	if !errors.As(err, &ce) || !errors.Is(err, limit.ErrConcurrencyExceeded) {
		t.Fatalf("err = %v, want a *ConcurrencyError that wraps ErrConcurrencyExceeded", err)
	}
	if ce.Team != team || ce.Limit != wantLimit {
		t.Errorf("ConcurrencyError = %+v, want team %q, limit %d", ce, team, wantLimit)
	}
	if errors.Is(err, limit.ErrBudgetExceeded) {
		t.Errorf("err = %v also wraps ErrBudgetExceeded", err)
	}
}

func (s suite) testInflightCap(t *testing.T) {
	l, _ := s.newCapLimiter(t, 2)
	ctx := t.Context()

	if _, err := l.Reserve(ctx, "team-b", 10); err != nil {
		t.Fatalf("team-b: Reserve: %v", err)
	}
	r1 := mustReserve(t, l, 10)
	r2 := mustReserve(t, l, 10)
	_, err := l.Reserve(ctx, team, 10)
	assertConcurrencyError(t, err, 2)
	if err := r2.TopUp(ctx, 5); err != nil {
		t.Fatalf("TopUp at the in-flight limit: %v", err)
	}

	if err := r1.Settle(ctx, 10); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	r3 := mustReserve(t, l, 10)
	for _, r := range []limit.Reservation{r2, r3} {
		if err := r.Settle(ctx, 10); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}
	// The denied Reserve held nothing.
	assertRemaining(t, l, 970)
}

// testInflightCapSpansWindows checks that a reservation that started in an
// earlier window still takes a slot.
func (s suite) testInflightCapSpansWindows(t *testing.T) {
	l, c := s.newCapLimiter(t, 1)
	r := mustReserve(t, l, 10)
	c.advance(window)
	_, err := l.Reserve(t.Context(), team, 10)
	assertConcurrencyError(t, err, 1)
	if err := r.Settle(t.Context(), 10); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	mustReserve(t, l, 10)
}

func (s suite) testNoInflightCapAtZero(t *testing.T) {
	l, _ := s.newCapLimiter(t, 0)
	for range 50 {
		mustReserve(t, l, 1)
	}
}

func (s suite) testConcurrentReserveNeverPassesInflightCap(t *testing.T) {
	const workers, maxInflight = 20, 5
	l, _ := s.newCapLimiter(t, maxInflight)
	ctx := t.Context()

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted []limit.Reservation
	)
	for range workers {
		wg.Go(func() {
			r, err := l.Reserve(ctx, team, 1)
			switch {
			case err == nil:
				mu.Lock()
				granted = append(granted, r)
				mu.Unlock()
			case !errors.Is(err, limit.ErrConcurrencyExceeded):
				t.Errorf("Reserve: %v", err)
			}
		})
	}
	wg.Wait()
	if len(granted) != maxInflight {
		t.Fatalf("%d reservations granted, want %d", len(granted), maxInflight)
	}
	for _, r := range granted {
		if err := r.Settle(ctx, 1); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}
	for range maxInflight {
		mustReserve(t, l, 1)
	}
}
