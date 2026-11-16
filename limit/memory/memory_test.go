package memory_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/memory"
)

const team = "team-a"

func newLimiter(t *testing.T, tokens int64) *memory.Limiter {
	t.Helper()
	l, err := memory.New(map[string]limit.Budget{team: {Tokens: tokens, Window: time.Minute}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func mustReserve(t *testing.T, l limit.Limiter, tokens int64) limit.Reservation {
	t.Helper()
	r, err := l.Reserve(t.Context(), team, tokens)
	if err != nil {
		t.Fatalf("Reserve(%d): %v", tokens, err)
	}
	return r
}

// assertRemaining checks that exactly want tokens are left in the current
// window: want tokens fit, and one more does not. It releases what it took.
func assertRemaining(t *testing.T, l limit.Limiter, want int64) {
	t.Helper()
	ctx := t.Context()
	if want > 0 {
		r, err := l.Reserve(ctx, team, want)
		if err != nil {
			t.Fatalf("Reserve(%d) with %d left: %v", want, want, err)
		}
		defer r.Settle(ctx, 0) //nolint:errcheck // the reservation is fresh, so Settle cannot fail.
	}
	if r, err := l.Reserve(ctx, team, 1); !errors.Is(err, limit.ErrBudgetExceeded) {
		if err == nil {
			_ = r.Settle(ctx, 0)
		}
		t.Fatalf("Reserve(1) beyond %d left: err = %v, want ErrBudgetExceeded", want, err)
	}
}

func TestNewRejectsBadBudgets(t *testing.T) {
	tests := []struct {
		name    string
		budgets map[string]limit.Budget
	}{
		{"empty team", map[string]limit.Budget{"": {Tokens: 1, Window: time.Minute}}},
		{"zero tokens", map[string]limit.Budget{team: {Tokens: 0, Window: time.Minute}}},
		{"negative tokens", map[string]limit.Budget{team: {Tokens: -1, Window: time.Minute}}},
		{"zero window", map[string]limit.Budget{team: {Tokens: 1, Window: 0}}},
		{"negative window", map[string]limit.Budget{team: {Tokens: 1, Window: -time.Minute}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := memory.New(tt.budgets); err == nil {
				t.Error("New returned no error")
			}
		})
	}
}

func TestReserve(t *testing.T) {
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
			l := newLimiter(t, 100)
			if tt.held > 0 {
				mustReserve(t, l, tt.held)
			}
			r, err := l.Reserve(t.Context(), tt.team, tt.tokens)
			checkErr(t, err, tt.wantErr)
			if err == nil && r.Reserved() != tt.tokens {
				t.Errorf("Reserved() = %d, want %d", r.Reserved(), tt.tokens)
			}
		})
	}
}

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

func TestReserveCanceledContext(t *testing.T) {
	l := newLimiter(t, 100)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Reserve(ctx, team, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertRemaining(t, l, 100)
}

func TestTopUp(t *testing.T) {
	l := newLimiter(t, 100)
	ctx := t.Context()
	r := mustReserve(t, l, 60)

	if err := r.TopUp(ctx, 30); err != nil {
		t.Fatalf("TopUp(30): %v", err)
	}
	if got := r.Reserved(); got != 90 {
		t.Fatalf("Reserved() = %d, want 90", got)
	}
	checkErr(t, r.TopUp(ctx, 11), limit.ErrBudgetExceeded)
	if got := r.Reserved(); got != 90 {
		t.Fatalf("Reserved() after failed TopUp = %d, want 90", got)
	}
	checkErr(t, r.TopUp(ctx, 0), errAny)

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

func TestSettle(t *testing.T) {
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
			l := newLimiter(t, 100)
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

func TestSettleHugeUsageSaturates(t *testing.T) {
	l := newLimiter(t, 100)
	rs := []limit.Reservation{mustReserve(t, l, 1), mustReserve(t, l, 1)}
	for _, r := range rs {
		if err := r.Settle(t.Context(), math.MaxInt64); err != nil {
			t.Fatalf("Settle: %v", err)
		}
	}
	assertRemaining(t, l, 0)
}

func TestSettleTwiceOrNegative(t *testing.T) {
	l := newLimiter(t, 100)
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

func TestRolloverSettlesInStartWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := newLimiter(t, 100)
		ctx := t.Context()
		r := mustReserve(t, l, 70)
		start := limit.WindowOf(time.Now(), time.Minute)
		if got := r.Window(); got != start {
			t.Fatalf("Window() = %v, want %v", got, start)
		}

		time.Sleep(time.Minute)
		assertRemaining(t, l, 100)

		if err := r.TopUp(ctx, 30); err != nil {
			t.Fatalf("TopUp in the old window: %v", err)
		}
		checkErr(t, r.TopUp(ctx, 1), limit.ErrBudgetExceeded)
		if err := r.Settle(ctx, 100); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if got := r.Window(); got != start {
			t.Errorf("Window() after rollover = %v, want %v", got, start)
		}
		assertRemaining(t, l, 100)
	})
}

func TestConcurrentReserveNeverPassesBudget(t *testing.T) {
	const budget, callers = 50, 200
	l := newLimiter(t, budget)
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
		wg.Go(func() { _ = r.Settle(ctx, 0) })
	}
	wg.Wait()
	assertRemaining(t, l, budget)
}

// TestConcurrentTopUpOverspendBound checks the bound from the design: held
// tokens never pass the budget, and the settled total passes it by at most
// the sum of each stream's usage beyond what it held.
func TestConcurrentTopUpOverspendBound(t *testing.T) {
	const budget, streams, chunk, topUps = 1000, 20, 10, 5
	l := newLimiter(t, budget)
	ctx := t.Context()

	var (
		wg                  sync.WaitGroup
		mu                  sync.Mutex
		held, used, overage int64
		errs                []error
	)
	for i := range streams {
		wg.Go(func() {
			r, err := l.Reserve(ctx, team, chunk)
			if err != nil {
				if !errors.Is(err, limit.ErrBudgetExceeded) {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				return
			}
			for range topUps {
				if err := r.TopUp(ctx, chunk); err != nil {
					if !errors.Is(err, limit.ErrBudgetExceeded) {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
					break
				}
			}
			h := r.Reserved()
			extra := int64(i % 3)
			if err := r.Settle(ctx, h+extra); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
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
