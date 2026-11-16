// Package memory is a [limit.Limiter] that keeps all state in one process.
//
// It suits tests and a single gateway replica. Replicas that share budgets
// need a distributed limiter instead.
package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
)

// Limiter is an in-process [limit.Limiter]. It is safe for concurrent use.
type Limiter struct {
	budgets map[string]limit.Budget
	now     func() time.Time

	mu sync.Mutex
	// windows maps team, then window start, to that window's counter.
	windows map[string]map[time.Time]*counter
}

type counter struct {
	settled  int64
	reserved int64
	// open counts reservations that have not settled; a window with open
	// reservations must outlive its end.
	open int
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

// New returns a limiter that enforces budgets, keyed by team. It returns an
// error for an empty team name or a budget whose tokens or window is not
// positive.
func New(budgets map[string]limit.Budget, opts ...Option) (*Limiter, error) {
	l := &Limiter{
		budgets: make(map[string]limit.Budget, len(budgets)),
		now:     time.Now,
		windows: make(map[string]map[time.Time]*counter, len(budgets)),
	}
	for _, opt := range opts {
		opt(l)
	}
	for team, b := range budgets {
		switch {
		case team == "":
			return nil, errors.New("memory: team name is empty")
		case b.Tokens <= 0:
			return nil, fmt.Errorf("memory: team %q: tokens must be positive", team)
		case b.Window <= 0:
			return nil, fmt.Errorf("memory: team %q: window must be positive", team)
		}
		l.budgets[team] = b
	}
	return l, nil
}

// Reserve implements [limit.Limiter].
func (l *Limiter) Reserve(ctx context.Context, team string, tokens int64) (limit.Reservation, error) {
	if tokens <= 0 {
		return nil, fmt.Errorf("memory: reserve %d tokens: must be positive", tokens)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, ok := l.budgets[team]
	if !ok {
		return nil, fmt.Errorf("memory: team %q: %w", team, limit.ErrNoBudget)
	}
	now := l.now()
	w := limit.WindowOf(now, b.Window)

	l.mu.Lock()
	defer l.mu.Unlock()
	teamWindows := l.windows[team]
	if teamWindows == nil {
		teamWindows = make(map[time.Time]*counter)
		l.windows[team] = teamWindows
	}
	prune(teamWindows, b.Window, now)
	c := teamWindows[w.Start]
	if c == nil {
		c = &counter{}
		teamWindows[w.Start] = c
	}
	if !fits(c, tokens, b.Tokens) {
		return nil, fmt.Errorf("memory: reserve: %w", &limit.ExceededError{Team: team, Tokens: tokens, Window: w})
	}
	c.reserved += tokens
	c.open++
	return &reservation{l: l, team: team, budget: b.Tokens, window: w, c: c, held: tokens}, nil
}

// prune drops the windows that have ended and have no open reservation. The
// caller holds l.mu.
func prune(windows map[time.Time]*counter, length time.Duration, now time.Time) {
	for start, c := range windows {
		if c.open == 0 && !start.Add(length).After(now) {
			delete(windows, start)
		}
	}
}

// fits reports whether tokens more fit in the window. It subtracts instead
// of adding so that a huge tokens value cannot overflow and pass.
func fits(c *counter, tokens, budget int64) bool {
	return tokens <= budget-c.settled-c.reserved
}

type reservation struct {
	l      *Limiter
	team   string
	budget int64
	window limit.Window
	// c stays valid after rollover: prune never deletes a window while
	// this reservation is open.
	c *counter

	// held and done are guarded by l.mu.
	held int64
	done bool
}

// Window implements [limit.Reservation].
func (r *reservation) Window() limit.Window { return r.window }

// Reserved implements [limit.Reservation].
func (r *reservation) Reserved() int64 {
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	return r.held
}

// TopUp implements [limit.Reservation].
func (r *reservation) TopUp(ctx context.Context, tokens int64) error {
	if tokens <= 0 {
		return fmt.Errorf("memory: top up %d tokens: must be positive", tokens)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	if r.done {
		return fmt.Errorf("memory: top up: %w", limit.ErrReservationClosed)
	}
	if !fits(r.c, tokens, r.budget) {
		return fmt.Errorf("memory: top up: %w", &limit.ExceededError{Team: r.team, Tokens: tokens, Window: r.window})
	}
	r.c.reserved += tokens
	r.held += tokens
	return nil
}

// Settle implements [limit.Reservation]. It ignores ctx: the state is
// local, so nothing can block.
func (r *reservation) Settle(_ context.Context, used int64) error {
	if used < 0 {
		return fmt.Errorf("memory: settle %d tokens: must not be negative", used)
	}
	r.l.mu.Lock()
	defer r.l.mu.Unlock()
	if r.done {
		return fmt.Errorf("memory: settle: %w", limit.ErrReservationClosed)
	}
	r.done = true
	r.c.reserved -= r.held
	// Saturate so that huge reported usage cannot wrap settled negative
	// and reopen the window.
	r.c.settled = min(r.c.settled, math.MaxInt64-used) + used
	r.c.open--
	r.held = 0
	return nil
}
