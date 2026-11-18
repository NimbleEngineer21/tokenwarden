package postgres

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"maps"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

const (
	// pruneEvery is how often Run deletes old usage windows. Old windows
	// cost only disk space, so this need not follow the lease TTL.
	pruneEvery = time.Minute
	// reapBatch is the most reservations that one reap transaction claims.
	reapBatch = 100
)

// Run renews the leases of this limiter's open reservations, reaps the
// reservations of every replica whose leases ended, and deletes the usage
// counts of windows that ended more than the retention ago and the usage
// ledger rows older than the ledger retention. It returns when
// ctx is done. It logs failures and keeps running, so that a short database
// outage does not stop it. Call Run once per Limiter.
//
// Every Limiter that reserves must run Run, and keep it running until its
// last reservation settles. Without it, the Run loop of another Limiter on
// the database reaps the open reservations although their requests still
// run.
func (l *Limiter) Run(ctx context.Context) {
	runTasks(ctx, l.log, []task{
		{"renew leases", l.ttl / 3, l.renew},
		{"reap expired reservations", l.ttl / 3, l.reap},
		{"prune usage windows", pruneEvery, l.prune},
	})
}

// task is work that Run does at a fixed interval.
type task struct {
	name  string
	every time.Duration
	run   func(context.Context) error
}

// runTasks runs each task at its interval until ctx is done. A slow task
// does not delay the others.
func runTasks(ctx context.Context, log *slog.Logger, tasks []task) {
	var wg sync.WaitGroup
	for _, t := range tasks {
		wg.Go(func() {
			tick := time.NewTicker(t.every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
				if err := t.run(ctx); err != nil && ctx.Err() == nil {
					log.ErrorContext(ctx, "lease task failed", "task", t.name, "err", err)
				}
			}
		})
	}
	wg.Wait()
}

// renew extends the leases of this limiter's open reservations.
func (l *Limiter) renew(ctx context.Context) error {
	l.mu.Lock()
	ids := slices.Collect(maps.Keys(l.open))
	l.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	return l.q.RenewLeases(ctx, db.RenewLeasesParams{ExpiresAt: l.now().Add(l.ttl), Ids: ids})
}

// reap claims expired reservations in batches until none are left.
func (l *Limiter) reap(ctx context.Context) error {
	for {
		n, tokens, err := l.reapBatch(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			l.log.WarnContext(ctx, "reaped reservations whose lease ended", "reservations", n, "tokens", tokens)
			if l.onReap != nil {
				l.onReap(n, tokens)
			}
		}
		if n < reapBatch {
			return nil
		}
	}
}

// windowKey names one usage window row.
type windowKey struct {
	team    string
	startUs int64
	us      int64
}

// ledgerKey names one usage ledger row.
type ledgerKey struct {
	team   string
	hourUs int64
	keyID  string
	model  string
}

// ledgerCharge is what reaped reservations add to one ledger row.
type ledgerCharge struct {
	requests int64
	tokens   int64
}

// reapBatch deletes up to reapBatch expired reservations, charges what they
// held to their windows and to the usage ledger, and frees their in-flight
// slots, in one transaction. It updates the windows in key order, then the
// budgets in team order, then the ledger rows in key order, so that two
// replicas that reap at the same time cannot deadlock, and a Settle or
// Reserve, which lock in the same order, cannot deadlock with it.
func (l *Limiter) reapBatch(ctx context.Context) (n int, tokens int64, err error) {
	err = pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		q := l.q.WithTx(tx)
		rows, err := q.ClaimExpired(ctx, db.ClaimExpiredParams{Now: l.now(), MaxRows: reapBatch})
		if err != nil {
			return err
		}
		held := make(map[windowKey]int64)
		slots := make(map[string]int64)
		ledger := make(map[ledgerKey]ledgerCharge)
		for _, r := range rows {
			slots[r.Team]++
			k := windowKey{r.Team, r.WindowStart.UnixMicro(), r.WindowUs}
			// The sum fits: it is at most the window's reserved count.
			held[k] += r.Held
			// An hour's row can take tokens from two windows when the
			// budget's window length changed, so this sum saturates.
			lk := ledgerKey{r.Team, r.Hour.UnixMicro(), r.KeyID, r.Model}
			c := ledger[lk]
			ledger[lk] = ledgerCharge{c.requests + 1, min(c.tokens, math.MaxInt64-r.Held) + r.Held}
		}
		keys := slices.SortedFunc(maps.Keys(held), func(a, b windowKey) int {
			return cmp.Or(cmp.Compare(a.team, b.team), cmp.Compare(a.startUs, b.startUs), cmp.Compare(a.us, b.us))
		})
		tokens = 0
		for _, k := range keys {
			err := q.ChargeHeld(ctx, db.ChargeHeldParams{
				Held:        held[k],
				Team:        k.team,
				WindowStart: time.UnixMicro(k.startUs).UTC(),
				WindowUs:    k.us,
			})
			if err != nil {
				return err
			}
			tokens = min(tokens, math.MaxInt64-held[k]) + held[k]
		}
		for _, team := range slices.Sorted(maps.Keys(slots)) {
			if err := q.ReleaseSlots(ctx, db.ReleaseSlotsParams{N: slots[team], Team: team}); err != nil {
				return err
			}
		}
		lkeys := slices.SortedFunc(maps.Keys(ledger), func(a, b ledgerKey) int {
			return cmp.Or(cmp.Compare(a.team, b.team), cmp.Compare(a.hourUs, b.hourUs),
				cmp.Compare(a.keyID, b.keyID), cmp.Compare(a.model, b.model))
		})
		for _, k := range lkeys {
			err := q.ChargeLedgerReaped(ctx, db.ChargeLedgerReapedParams{
				Team:     k.team,
				Hour:     time.UnixMicro(k.hourUs).UTC(),
				KeyID:    k.keyID,
				Model:    k.model,
				Requests: ledger[k].requests,
				Reaped:   ledger[k].tokens,
			})
			if err != nil {
				return err
			}
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return n, tokens, nil
}

// prune deletes the usage counts of windows that ended more than the
// retention ago, and the usage ledger rows of hours that started more than
// the ledger retention ago.
func (l *Limiter) prune(ctx context.Context) error {
	now := l.now()
	_, err := l.q.PruneWindows(ctx, now.Add(-l.retention))
	_, lerr := l.q.PruneLedger(ctx, now.Add(-l.ledgerRetention))
	return errors.Join(err, lerr)
}
