package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/internal/store"
	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// clock is a test clock that starts far from the real date, so that code
// that reads time.Now cannot pass by chance.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2001, 2, 3, 4, 5, 15, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// reaps records the calls of a reap func.
type reaps struct {
	reservations atomic.Int64
	tokens       atomic.Int64
}

func (r *reaps) record(n int, tokens int64) {
	r.reservations.Add(int64(n))
	r.tokens.Add(tokens)
}

func migratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.New(t)
	if err := store.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

// newLeaseLimiter returns a limiter on pool with a 30 s lease, the clock c,
// and a budget of 1,000,000 tokens per minute for team-a.
func newLeaseLimiter(t *testing.T, pool *pgxpool.Pool, c *clock, r *reaps, opts ...Option) *Limiter {
	t.Helper()
	opts = append([]Option{
		WithClock(c.now),
		WithLeaseTTL(30 * time.Second),
		WithReapFunc(r.record),
		WithLogger(slog.New(slog.DiscardHandler)),
	}, opts...)
	l := New(pool, opts...)
	if err := l.SetBudget(t.Context(), "team-a", limit.Budget{Tokens: 1_000_000, Window: time.Minute}); err != nil {
		t.Fatalf("SetBudget: %v", err)
	}
	return l
}

// usage returns the settled and reserved counts of team-a, summed over its
// windows.
func usage(t *testing.T, pool *pgxpool.Pool) (settled, reserved int64) {
	t.Helper()
	err := pool.QueryRow(t.Context(),
		"SELECT coalesce(sum(settled), 0)::bigint, coalesce(sum(reserved), 0)::bigint FROM usage_windows WHERE team = 'team-a'",
	).Scan(&settled, &reserved)
	if err != nil {
		t.Fatalf("read usage: %v", err)
	}
	return settled, reserved
}

func mustReap(t *testing.T, l *Limiter) {
	t.Helper()
	if err := l.reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}
}

func TestReapChargesExpiredHold(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	res, err := l.Reserve(ctx, "team-a", 40)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	c.add(30 * time.Second)
	mustReap(t, l)
	if n := r.reservations.Load(); n != 0 {
		t.Fatalf("reaped %d reservations at the end of the lease, want 0", n)
	}

	c.add(time.Microsecond)
	mustReap(t, l)
	if n, tok := r.reservations.Load(), r.tokens.Load(); n != 1 || tok != 40 {
		t.Fatalf("reaped %d reservations, %d tokens; want 1, 40", n, tok)
	}
	if s, rv := usage(t, pool); s != 40 || rv != 0 {
		t.Fatalf("settled, reserved = %d, %d; want 40, 0", s, rv)
	}
	var inflight int64
	if err := pool.QueryRow(ctx, "SELECT inflight FROM budgets WHERE team = 'team-a'").Scan(&inflight); err != nil {
		t.Fatalf("read inflight: %v", err)
	}
	if inflight != 0 {
		t.Errorf("inflight = %d after the reap, want 0", inflight)
	}
	if err := res.TopUp(ctx, 1); !errors.Is(err, limit.ErrReservationClosed) {
		t.Errorf("TopUp after reap: err = %v, want ErrReservationClosed", err)
	}
	if err := res.Settle(ctx, 5); !errors.Is(err, limit.ErrReservationClosed) {
		t.Errorf("Settle after reap: err = %v, want ErrReservationClosed", err)
	}
	if s, _ := usage(t, pool); s != 40 {
		t.Errorf("settled = %d after the late Settle, want 40", s)
	}
}

func TestRenewKeepsLease(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	if _, err := l.Reserve(ctx, "team-a", 40); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	c.add(20 * time.Second)
	if err := l.renew(ctx); err != nil {
		t.Fatalf("renew: %v", err)
	}
	c.add(20 * time.Second)
	mustReap(t, l)
	if n := r.reservations.Load(); n != 0 {
		t.Fatalf("reaped %d renewed reservations, want 0", n)
	}
	c.add(11 * time.Second)
	mustReap(t, l)
	if n := r.reservations.Load(); n != 1 {
		t.Fatalf("reaped %d reservations after the renewed lease ended, want 1", n)
	}
}

// TestRenewNeverShortens checks that a renewal from a clock that is behind
// keeps the longer lease.
func TestRenewNeverShortens(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	if _, err := l.Reserve(ctx, "team-a", 40); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	c.add(-20 * time.Second)
	if err := l.renew(ctx); err != nil {
		t.Fatalf("renew: %v", err)
	}
	c.add(31 * time.Second)
	mustReap(t, l)
	if n := r.reservations.Load(); n != 0 {
		t.Fatalf("reaped %d reservations; a renewal shortened the lease", n)
	}
}

func TestTopUpExtendsLease(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	res, err := l.Reserve(ctx, "team-a", 40)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	c.add(20 * time.Second)
	if err := res.TopUp(ctx, 10); err != nil {
		t.Fatalf("TopUp: %v", err)
	}
	c.add(20 * time.Second)
	mustReap(t, l)
	if n := r.reservations.Load(); n != 0 {
		t.Fatalf("reaped %d reservations after a top-up, want 0", n)
	}
	c.add(11 * time.Second)
	mustReap(t, l)
	if n, tok := r.reservations.Load(), r.tokens.Load(); n != 1 || tok != 50 {
		t.Fatalf("reaped %d reservations, %d tokens; want 1, 50", n, tok)
	}
}

// TestRenewSkipsSettled checks that Run stops renewing a reservation after
// any Settle, a failed one too, so that a failed settle is reaped.
func TestRenewSkipsSettled(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	ok, err := l.Reserve(ctx, "team-a", 10)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	failed, err := l.Reserve(ctx, "team-a", 20)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := ok.Settle(ctx, 10); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := failed.Settle(canceled, 20); err == nil {
		t.Fatal("Settle with a canceled context returned no error")
	}
	l.mu.Lock()
	open := len(l.open)
	l.mu.Unlock()
	if open != 0 {
		t.Fatalf("%d reservations still renewed after Settle, want 0", open)
	}

	c.add(20 * time.Second)
	if err := l.renew(ctx); err != nil {
		t.Fatalf("renew: %v", err)
	}
	c.add(11 * time.Second)
	mustReap(t, l)
	if n, tok := r.reservations.Load(), r.tokens.Load(); n != 1 || tok != 20 {
		t.Fatalf("reaped %d reservations, %d tokens; want 1, 20", n, tok)
	}
	if s, rv := usage(t, pool); s != 30 || rv != 0 {
		t.Fatalf("settled, reserved = %d, %d; want 30, 0", s, rv)
	}
}

// TestReplicasReapEachOnce checks that two replicas that reap at the same
// time charge each reservation once, over more than one batch and window.
func TestReplicasReapEachOnce(t *testing.T) {
	pool := migratedPool(t)
	other, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)
	c := newClock()
	var ra, rb reaps
	a := newLeaseLimiter(t, pool, c, &ra)
	b := newLeaseLimiter(t, other, c, &rb)
	ctx := t.Context()

	const n = 2*reapBatch + 50
	var want int64
	for i := range n {
		if i == n/2 {
			c.add(time.Minute) // a second window
		}
		tokens := int64(i%7 + 1)
		want += tokens
		if _, err := a.Reserve(ctx, "team-a", tokens); err != nil {
			t.Fatalf("Reserve %d: %v", i, err)
		}
	}
	c.add(time.Minute)

	var wg sync.WaitGroup
	for _, l := range []*Limiter{a, b} {
		wg.Go(func() {
			if err := l.reap(ctx); err != nil {
				t.Errorf("reap: %v", err)
			}
		})
	}
	wg.Wait()
	if got := ra.reservations.Load() + rb.reservations.Load(); got != n {
		t.Errorf("reaped %d reservations, want %d", got, n)
	}
	if got := ra.tokens.Load() + rb.tokens.Load(); got != want {
		t.Errorf("reaped %d tokens, want %d", got, want)
	}
	if s, rv := usage(t, pool); s != want || rv != 0 {
		t.Errorf("settled, reserved = %d, %d; want %d, 0", s, rv, want)
	}
}

func TestPrune(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r, WithRetention(time.Hour))
	ctx := t.Context()
	windows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM usage_windows").Scan(&n); err != nil {
			t.Fatalf("count windows: %v", err)
		}
		return n
	}

	// The clock starts 15 s into a minute window.
	settled, err := l.Reserve(ctx, "team-a", 10)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := settled.Settle(ctx, 10); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	c.add(time.Minute)
	if _, err := l.Reserve(ctx, "team-a", 10); err != nil { // stays open
		t.Fatalf("Reserve: %v", err)
	}

	// The first window ends 45 s after the start. One microsecond before
	// it is an hour old, it stays.
	c.add(45*time.Second + time.Hour - time.Minute - time.Microsecond)
	if err := l.prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := windows(); n != 2 {
		t.Fatalf("%d windows inside the retention, want 2", n)
	}
	c.add(time.Microsecond)
	if err := l.prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := windows(); n != 1 {
		t.Fatalf("%d windows after the first one aged out, want 1", n)
	}
	c.add(24 * time.Hour)
	if err := l.prune(ctx); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := windows(); n != 1 {
		t.Fatalf("%d windows; the window of an open reservation was pruned", n)
	}
}

func TestRunTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var fast, slow, failed atomic.Int64
		var logged atomic.Int64
		log := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
			logged.Add(1)
			return len(p), nil
		}), nil))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			runTasks(ctx, log, []task{
				{"fast", 10 * time.Second, func(context.Context) error { fast.Add(1); return nil }},
				{"slow", time.Minute, func(context.Context) error { slow.Add(1); return nil }},
				{"failed", 10 * time.Second, func(context.Context) error { failed.Add(1); return io.ErrUnexpectedEOF }},
			})
		}()

		time.Sleep(65 * time.Second)
		synctest.Wait()
		if f, s, e, l := fast.Load(), slow.Load(), failed.Load(), logged.Load(); f != 6 || s != 1 || e != 6 || l != 6 {
			t.Errorf("runs fast, slow, failed = %d, %d, %d, log lines = %d; want 6, 1, 6, 6", f, s, e, l)
		}
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("runTasks did not return after cancel")
		}
	})
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
