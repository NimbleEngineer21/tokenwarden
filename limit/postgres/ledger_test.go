package postgres

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// ledgerRow is one usage_ledger row.
type ledgerRow struct {
	Team       string
	Hour       time.Time
	KeyID      string
	Model      string
	Requests   int64
	Input      int64
	Output     int64
	CacheWrite int64
	CacheRead  int64
	Estimated  int64
	Reaped     int64
}

func ledgerRows(t *testing.T, pool *pgxpool.Pool) []ledgerRow {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT team, hour, key_id, model, requests, input_tokens, output_tokens,
		       cache_write_tokens, cache_read_tokens, estimated_tokens, reaped_tokens
		FROM usage_ledger ORDER BY team, hour, key_id, model`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		err := rows.Scan(&r.Team, &r.Hour, &r.KeyID, &r.Model, &r.Requests, &r.Input, &r.Output,
			&r.CacheWrite, &r.CacheRead, &r.Estimated, &r.Reaped)
		if err != nil {
			t.Fatalf("scan ledger: %v", err)
		}
		r.Hour = r.Hour.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return out
}

func checkLedger(t *testing.T, pool *pgxpool.Pool, want []ledgerRow) {
	t.Helper()
	if got := ledgerRows(t, pool); !slices.Equal(got, want) {
		t.Fatalf("ledger =\n%+v\nwant\n%+v", got, want)
	}
}

func reserveFor(t *testing.T, l *Limiter, tokens int64, a limit.Attribution) limit.UsageSettler {
	t.Helper()
	r, err := l.ReserveFor(t.Context(), "team-a", tokens, a)
	if err != nil {
		t.Fatalf("ReserveFor(%d, %+v): %v", tokens, a, err)
	}
	us, ok := r.(limit.UsageSettler)
	if !ok {
		t.Fatalf("reservation %T is not a limit.UsageSettler", r)
	}
	return us
}

func settleUsage(t *testing.T, r limit.UsageSettler, u limit.Usage) {
	t.Helper()
	if err := r.SettleUsage(t.Context(), u); err != nil {
		t.Fatalf("SettleUsage(%+v): %v", u, err)
	}
}

var (
	hour4 = time.Date(2001, 2, 3, 4, 0, 0, 0, time.UTC)
	hour5 = hour4.Add(time.Hour)
	attrA = limit.Attribution{KeyID: "0123456789ab", Model: "claude-test-a"}
	attrB = limit.Attribution{KeyID: "0123456789ab", Model: "claude-test-b"}
)

func TestSettleWritesLedger(t *testing.T) {
	pool := migratedPool(t)
	c := newClock() // 04:05:15
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)

	settleUsage(t, reserveFor(t, l, 100, attrA), limit.Usage{Input: 10, Output: 20, CacheWrite: 3, CacheRead: 4, Estimated: 5})
	if err := reserveFor(t, l, 100, attrA).Settle(t.Context(), 7); err != nil {
		t.Fatalf("Settle: %v", err)
	}
	settleUsage(t, reserveFor(t, l, 100, attrB), limit.Usage{}) // costs nothing: no row
	// A reservation that starts in one hour and settles in the next counts
	// in the hour where it started.
	c.add(54*time.Minute + 44*time.Second) // 04:59:59
	late := reserveFor(t, l, 100, attrB)
	c.add(2 * time.Second)
	settleUsage(t, late, limit.Usage{Output: 9})
	settleUsage(t, reserveFor(t, l, 100, attrB), limit.Usage{Input: 1})

	checkLedger(t, pool, []ledgerRow{
		{Team: "team-a", Hour: hour4, KeyID: attrA.KeyID, Model: attrA.Model, Requests: 2, Input: 10, Output: 20, CacheWrite: 3, CacheRead: 4, Estimated: 12},
		{Team: "team-a", Hour: hour4, KeyID: attrB.KeyID, Model: attrB.Model, Requests: 1, Output: 9},
		{Team: "team-a", Hour: hour5, KeyID: attrB.KeyID, Model: attrB.Model, Requests: 1, Input: 1},
	})
	if s, _ := usage(t, pool); s != 59 {
		t.Errorf("settled = %d, want 59, the ledger's sum", s)
	}
}

func TestSettleUsageRejectsNegative(t *testing.T) {
	pool := migratedPool(t)
	var r reaps
	l := newLeaseLimiter(t, pool, newClock(), &r)
	for _, u := range []limit.Usage{{Input: -1}, {Output: -1}, {CacheWrite: -1}, {CacheRead: -1}, {Estimated: -1}} {
		if err := reserveFor(t, l, 10, attrA).SettleUsage(t.Context(), u); err == nil {
			t.Errorf("SettleUsage(%+v) = nil, want an error", u)
		}
	}
	checkLedger(t, pool, nil)
}

func TestLedgerSaturates(t *testing.T) {
	pool := migratedPool(t)
	var r reaps
	c := newClock()
	l := newLeaseLimiter(t, pool, c, &r)
	big := limit.Usage{
		Input: math.MaxInt64, Output: math.MaxInt64, CacheWrite: math.MaxInt64,
		CacheRead: math.MaxInt64, Estimated: math.MaxInt64,
	}
	settleUsage(t, reserveFor(t, l, 10, attrA), big)
	c.add(time.Minute) // a new budget window in the same hour
	settleUsage(t, reserveFor(t, l, 10, attrA), big)
	checkLedger(t, pool, []ledgerRow{{
		Team: "team-a", Hour: hour4, KeyID: attrA.KeyID, Model: attrA.Model, Requests: 2,
		Input: math.MaxInt64, Output: math.MaxInt64, CacheWrite: math.MaxInt64,
		CacheRead: math.MaxInt64, Estimated: math.MaxInt64,
	}})
}

func TestReapWritesLedger(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	reserveFor(t, l, 40, attrA)
	reserveFor(t, l, 2, attrA)
	if _, err := l.Reserve(ctx, "team-a", 5); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	// A settle in the same row before the reap: the reap adds to it.
	settleUsage(t, reserveFor(t, l, 10, attrA), limit.Usage{Input: 3})
	c.add(time.Minute)
	mustReap(t, l)

	checkLedger(t, pool, []ledgerRow{
		{Team: "team-a", Hour: hour4, Requests: 1, Reaped: 5},
		{Team: "team-a", Hour: hour4, KeyID: attrA.KeyID, Model: attrA.Model, Requests: 3, Input: 3, Reaped: 42},
	})
}

// TestLedgerHourOfOldReplica checks the rows that a replica from before the
// ledger wrote: no key ID, no model, no hour. The hour comes from
// created_at in UTC, even when the session time zone has a half-hour
// offset.
func TestLedgerHourOfOldReplica(t *testing.T) {
	base := migratedPool(t)
	cfg := base.Config()
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	ctx := t.Context()

	settledRes := reserveFor(t, l, 10, attrA)
	reserveFor(t, l, 20, attrA) // reaped
	_, err = pool.Exec(ctx, `UPDATE reservations
		SET key_id = '', model = '', hour = NULL, created_at = '2001-02-03T04:10:00Z'`)
	if err != nil {
		t.Fatalf("make old rows: %v", err)
	}
	settleUsage(t, settledRes, limit.Usage{Output: 4})
	c.add(time.Minute)
	mustReap(t, l)
	checkLedger(t, pool, []ledgerRow{{Team: "team-a", Hour: hour4, Requests: 2, Output: 4, Reaped: 20}})
}

func TestReserveForCleansNames(t *testing.T) {
	pool := migratedPool(t)
	var r reaps
	l := newLeaseLimiter(t, pool, newClock(), &r)
	long := strings.Repeat("m", maxModelBytes)
	for _, name := range []string{long + "m", "a\x00b", "a\xffb", long} {
		settleUsage(t, reserveFor(t, l, 10, limit.Attribution{KeyID: "k", Model: name}), limit.Usage{Input: 1})
		settleUsage(t, reserveFor(t, l, 10, limit.Attribution{KeyID: name, Model: "m"}), limit.Usage{Output: 1})
	}
	checkLedger(t, pool, []ledgerRow{
		{Team: "team-a", Hour: hour4, Model: "m", Requests: 3, Output: 3},
		{Team: "team-a", Hour: hour4, KeyID: "k", Requests: 3, Input: 3},
		{Team: "team-a", Hour: hour4, KeyID: "k", Model: long, Requests: 1, Input: 1},
		{Team: "team-a", Hour: hour4, KeyID: long, Model: "m", Requests: 1, Output: 1},
	})
}

func TestPruneLedger(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r, WithLedgerRetention(2*time.Hour))
	settleUsage(t, reserveFor(t, l, 10, attrA), limit.Usage{Input: 1})

	// The row's hour started at 04:00; the clock is at 04:05:15.
	c.add(2*time.Hour - 5*time.Minute - 15*time.Second)
	if err := l.prune(t.Context()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if n := len(ledgerRows(t, pool)); n != 1 {
		t.Fatalf("%d ledger rows at the retention, want 1", n)
	}
	c.add(time.Microsecond)
	if err := l.prune(t.Context()); err != nil {
		t.Fatalf("prune: %v", err)
	}
	checkLedger(t, pool, nil)
}

// TestLedgerMatchesSettled runs settles and reaps at the same time and
// checks that the ledger sums to the settled counts of the windows.
func TestLedgerMatchesSettled(t *testing.T) {
	pool := migratedPool(t)
	c := newClock()
	var r reaps
	l := newLeaseLimiter(t, pool, c, &r)
	reaper := newLeaseLimiter(t, pool, c, &r)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			_ = reaper.reap(ctx)
		}
	})
	var workers sync.WaitGroup
	for w := range 8 {
		workers.Go(func() {
			a := limit.Attribution{KeyID: "key", Model: []string{"m1", "m2"}[w%2]}
			for i := range 30 {
				res, err := l.ReserveFor(ctx, "team-a", 50, a)
				if err != nil {
					t.Errorf("ReserveFor: %v", err)
					return
				}
				if i%3 == 0 {
					continue // left for the reaper
				}
				n := int64(w + i)
				err = res.(limit.UsageSettler).SettleUsage(ctx, limit.Usage{Input: n, Output: 2 * n, CacheRead: 1, Estimated: int64(i % 2)})
				// The reaper can take a reservation whose lease ended
				// before its settle.
				if err != nil && !errors.Is(err, limit.ErrReservationClosed) {
					t.Errorf("SettleUsage: %v", err)
				}
				if i == 15 {
					c.add(31 * time.Second) // ends the leases of the open ones
				}
			}
		})
	}
	workers.Wait()
	c.add(time.Minute)
	cancel()
	wg.Wait()
	mustReap(t, l)

	var sum int64
	for _, row := range ledgerRows(t, pool) {
		sum += row.Input + row.Output + row.CacheWrite + row.CacheRead + row.Estimated + row.Reaped
	}
	settled, reserved := usage(t, pool)
	if sum != settled || reserved != 0 {
		t.Fatalf("ledger sum = %d, settled = %d, reserved = %d; want equal sums and 0 reserved", sum, settled, reserved)
	}
}
