// Package postgres is a [limit.Limiter] that keeps budgets and usage in
// Postgres, so that gateway replicas share them.
//
// Each Reserve, TopUp, and Settle is one conditional statement on the
// team's row for the window, so concurrent replicas can never hold more
// than the budget. The window of a new reservation comes from the Go clock
// of the replica, not from the database clock; replica clocks must agree to
// well within a window length.
//
// The database must have the tokenwarden schema; [Migrate] creates it.
package postgres

import (
	"context"
	"errors"
	"fmt"
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
	q   *db.Queries
	now func() time.Time
}

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

// New returns a limiter that uses pool.
func New(pool *pgxpool.Pool, opts ...Option) *Limiter {
	l := &Limiter{q: db.New(pool), now: time.Now}
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
	w := limit.WindowOf(l.now(), time.Duration(b.WindowUs)*time.Microsecond)

	err = l.q.EnsureWindow(ctx, db.EnsureWindowParams{Team: team, WindowStart: w.Start, WindowUs: b.WindowUs})
	if err != nil {
		return nil, fmt.Errorf("postgres: team %q: create window: %w", team, err)
	}
	id, err := l.q.Reserve(ctx, db.ReserveParams{
		Budget:      b.Tokens,
		Tokens:      tokens,
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
	return &reservation{q: l.q, id: id, team: team, window: w, held: tokens}, nil
}

type reservation struct {
	q      *db.Queries
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
	res, err := r.q.TopUp(ctx, db.TopUpParams{ID: r.id, Tokens: tokens})
	switch {
	case err != nil:
		return fmt.Errorf("postgres: team %q: top up %d tokens: %w", r.team, tokens, err)
	case !res.Open:
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

// Settle implements [limit.Reservation].
func (r *reservation) Settle(ctx context.Context, used int64) error {
	if used < 0 {
		return fmt.Errorf("postgres: settle %d tokens: must not be negative", used)
	}
	_, err := r.q.Settle(ctx, db.SettleParams{ID: r.id, Used: used})
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
