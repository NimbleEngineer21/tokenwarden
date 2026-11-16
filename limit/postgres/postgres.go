// Package postgres is a [limit.Limiter] that keeps budgets and usage in
// Postgres, so that gateway replicas share them.
//
// Each Reserve, TopUp, and Settle is one conditional statement on the
// team's row for the window, so concurrent replicas can never hold more
// than the budget. The window of a new reservation comes from the Go clock
// of the replica, not from the database clock; replica clocks must agree to
// well within a window length.
//
// Each reservation has a lease. The replica that holds it renews the lease
// while the reservation is open, from [Limiter.Run]. When a replica stops
// without settling, for example in a crash, its leases end and the Run loop
// of any replica reaps the reservations: it charges their held tokens to
// their windows, since the requests may have used them upstream. Leases use
// the Go clock too.
//
// The database must have the tokenwarden schema; [Migrate] creates it.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/store"
	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// Migrate creates or updates the tokenwarden schema in the database of
// pool. Run it before the first [New]. Replicas that start at the same time
// can all call it: a lock makes them wait for each other. It returns an
// error if a newer release already migrated the database.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return store.Migrate(ctx, pool)
}

// Limiter is a [limit.Limiter] backed by Postgres. It is safe for
// concurrent use, and any number of Limiters can share one database.
type Limiter struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	now       func() time.Time
	ttl       time.Duration
	retention time.Duration
	onReap    func(reservations int, tokens int64)
	log       *slog.Logger

	// open holds the ids of this limiter's open reservations, whose leases
	// Run renews.
	mu   sync.Mutex
	open map[int64]struct{}
}

// Defaults of the lease options.
const (
	DefaultLeaseTTL  = 30 * time.Second
	DefaultRetention = 7 * 24 * time.Hour
)

// Option configures a [Limiter].
type Option func(*Limiter)

// WithClock makes the limiter read the time from now instead of
// [time.Now]. The time picks the window of each new reservation. A nil now
// keeps [time.Now].
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) {
		if now != nil {
			l.now = now
		}
	}
}

// WithLeaseTTL sets how long a reservation's lease lasts without a renewal.
// Run renews leases and reaps ended ones every third of it, so a stopped
// replica's reservations are reaped between ttl and 4/3 ttl after its last
// renewal. Values below one millisecond keep [DefaultLeaseTTL].
func WithLeaseTTL(ttl time.Duration) Option {
	return func(l *Limiter) {
		if ttl >= time.Millisecond {
			l.ttl = ttl
		}
	}
}

// WithRetention sets how long Run keeps the usage counts of a window after
// the window ends. A negative value keeps [DefaultRetention].
func WithRetention(d time.Duration) Option {
	return func(l *Limiter) {
		if d >= 0 {
			l.retention = d
		}
	}
}

// WithReapFunc makes Run call f after it reaps reservations, with how many
// it reaped and the tokens that they held. A nil f calls nothing.
func WithReapFunc(f func(reservations int, tokens int64)) Option {
	return func(l *Limiter) { l.onReap = f }
}

// WithLogger makes Run log reaps and failures to log. A nil log keeps
// [slog.Default].
func WithLogger(log *slog.Logger) Option {
	return func(l *Limiter) {
		if log != nil {
			l.log = log
		}
	}
}

// New returns a limiter that uses pool.
func New(pool *pgxpool.Pool, opts ...Option) *Limiter {
	l := &Limiter{
		pool:      pool,
		q:         db.New(pool),
		now:       time.Now,
		ttl:       DefaultLeaseTTL,
		retention: DefaultRetention,
		log:       slog.Default(),
		open:      make(map[int64]struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// SetBudget creates or replaces the budget of team. Open reservations keep
// the budget that they started with; new reservations use the new one. A
// new window length starts a new count at once. It returns an error for an
// empty team, tokens that are not positive, or a window that is not a
// positive whole number of microseconds.
func (l *Limiter) SetBudget(ctx context.Context, team string, b limit.Budget) error {
	switch {
	case team == "":
		return errors.New("postgres: team name is empty")
	case b.Tokens <= 0:
		return fmt.Errorf("postgres: team %q: tokens must be positive", team)
	case b.Window <= 0 || b.Window%time.Microsecond != 0:
		return fmt.Errorf("postgres: team %q: window must be a positive whole number of microseconds", team)
	}
	err := l.q.UpsertBudget(ctx, db.UpsertBudgetParams{
		Team:     team,
		Tokens:   b.Tokens,
		WindowUs: b.Window.Microseconds(),
	})
	if err != nil {
		return fmt.Errorf("postgres: team %q: set budget: %w", team, err)
	}
	return nil
}

// Reserve implements [limit.Limiter].
func (l *Limiter) Reserve(ctx context.Context, team string, tokens int64) (limit.Reservation, error) {
	if tokens <= 0 {
		return nil, fmt.Errorf("postgres: reserve %d tokens: must be positive", tokens)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, err := l.q.GetBudget(ctx, team)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: team %q: %w", team, limit.ErrNoBudget)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: team %q: read budget: %w", team, err)
	}
	now := l.now()
	w := limit.WindowOf(now, time.Duration(b.WindowUs)*time.Microsecond)

	err = l.q.EnsureWindow(ctx, db.EnsureWindowParams{Team: team, WindowStart: w.Start, WindowUs: b.WindowUs})
	if err != nil {
		return nil, fmt.Errorf("postgres: team %q: create window: %w", team, err)
	}
	id, err := l.q.Reserve(ctx, db.ReserveParams{
		Budget:      b.Tokens,
		Tokens:      tokens,
		ExpiresAt:   now.Add(l.ttl),
		Team:        team,
		WindowStart: w.Start,
		WindowUs:    b.WindowUs,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: %w", &limit.ExceededError{Team: team, Tokens: tokens, Window: w})
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: team %q: reserve %d tokens: %w", team, tokens, err)
	}
	l.track(id, true)
	return &reservation{l: l, id: id, team: team, window: w, held: tokens}, nil
}

// track adds id to or drops it from the reservations that Run renews.
func (l *Limiter) track(id int64, open bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if open {
		l.open[id] = struct{}{}
	} else {
		delete(l.open, id)
	}
}

type reservation struct {
	l      *Limiter
	id     int64
	team   string
	window limit.Window

	// The database is the source of truth; held and done mirror it so that
	// Reserved needs no query. mu is never held during a query.
	mu   sync.Mutex
	held int64
	done bool
}

// Window implements [limit.Reservation].
func (r *reservation) Window() limit.Window { return r.window }

// Reserved implements [limit.Reservation].
func (r *reservation) Reserved() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}

// TopUp implements [limit.Reservation].
func (r *reservation) TopUp(ctx context.Context, tokens int64) error {
	if tokens <= 0 {
		return fmt.Errorf("postgres: top up %d tokens: must be positive", tokens)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	res, err := r.l.q.TopUp(ctx, db.TopUpParams{ID: r.id, Tokens: tokens, ExpiresAt: r.l.now().Add(r.l.ttl)})
	switch {
	case err != nil:
		return fmt.Errorf("postgres: team %q: top up %d tokens: %w", r.team, tokens, err)
	case !res.Open:
		// Settled, or reaped after its lease ended.
		r.l.track(r.id, false)
		return fmt.Errorf("postgres: top up: %w", limit.ErrReservationClosed)
	case !res.Granted:
		return fmt.Errorf("postgres: top up: %w", &limit.ExceededError{Team: r.team, Tokens: tokens, Window: r.window})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A Settle that ran after this top-up in the database released these
	// tokens too.
	if !r.done {
		r.held += tokens
	}
	return nil
}

// Settle implements [limit.Reservation]. After any Settle, Run stops
// renewing the lease: a reservation whose settle failed is reaped, so its
// tokens do not stay held for good.
func (r *reservation) Settle(ctx context.Context, used int64) error {
	if used < 0 {
		return fmt.Errorf("postgres: settle %d tokens: must not be negative", used)
	}
	defer r.l.track(r.id, false)
	_, err := r.l.q.Settle(ctx, db.SettleParams{ID: r.id, Used: used})
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: settle: %w", limit.ErrReservationClosed)
	}
	if err != nil {
		return fmt.Errorf("postgres: team %q: settle %d tokens: %w", r.team, used, err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done = true
	r.held = 0
	return nil
}
