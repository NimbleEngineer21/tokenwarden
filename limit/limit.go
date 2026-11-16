// Package limit defines token budgets per team and the [Limiter] that
// enforces them.
//
// A gateway calls [Limiter.Reserve] before it forwards a request, holds the
// returned [Reservation] while the reply streams, calls
// [Reservation.TopUp] before the output passes the held amount, and calls
// [Reservation.Settle] with the real usage when the reply ends. A
// reservation always settles in the [Window] where it started, even after
// that window has ended.
//
// Windows are fixed and aligned to the Unix epoch in UTC, so a 24-hour
// window starts at UTC midnight. Calendar months are not supported.
package limit

import (
	"context"
	"errors"
	"time"
)

// ErrBudgetExceeded means that the tokens asked for do not fit in what is
// left of the team's budget for the window.
var ErrBudgetExceeded = errors.New("limit: token budget exceeded")

// ErrNoBudget means that the team has no budget. A [Limiter] fails closed
// for such a team: it reserves nothing.
var ErrNoBudget = errors.New("limit: no budget for team")

// ErrReservationClosed means that the [Reservation] was already settled.
var ErrReservationClosed = errors.New("limit: reservation already settled")

// Budget is the token limit of one team per fixed window.
type Budget struct {
	// Tokens is the most tokens that the team can use in one window. It
	// must be positive.
	Tokens int64
	// Window is the length of one window. It must be positive.
	Window time.Duration
}

// Window is one fixed budget window.
type Window struct {
	// Start is the first instant of the window, in UTC.
	Start time.Time
	// Length is the duration of the window.
	Length time.Duration
}

// WindowOf returns the window of the given length that contains t. Windows
// are aligned to the Unix epoch. WindowOf panics if length is not positive.
func WindowOf(t time.Time, length time.Duration) Window {
	if length <= 0 {
		panic("limit: window length must be positive")
	}
	ns := t.UnixNano()
	off := ns % int64(length)
	if off < 0 {
		off += int64(length)
	}
	return Window{Start: time.Unix(0, ns-off).UTC(), Length: length}
}

// End returns the first instant after the window.
func (w Window) End() time.Time {
	return w.Start.Add(w.Length)
}

// Limiter reserves tokens against team budgets. Implementations are safe for
// concurrent use.
type Limiter interface {
	// Reserve holds tokens in the team's current window. It returns an
	// error that wraps [ErrBudgetExceeded] when settled plus reserved plus
	// tokens would pass the budget, and one that wraps [ErrNoBudget] when
	// the team has no budget. tokens must be positive.
	Reserve(ctx context.Context, team string, tokens int64) (Reservation, error)
}

// Reservation is a hold on part of one team's budget in one window. Its
// methods are safe for concurrent use.
type Reservation interface {
	// Window returns the window where the reservation started. The
	// reservation counts against this window until it settles.
	Window() Window
	// Reserved returns the number of tokens held now.
	Reserved() int64
	// TopUp holds more tokens in the reservation's own window. It returns
	// an error that wraps [ErrBudgetExceeded] when they do not fit; the
	// reservation then stays open and must still be settled. tokens must
	// be positive.
	TopUp(ctx context.Context, tokens int64) error
	// Settle ends the reservation. It frees the held tokens and charges
	// used tokens to the window, so used can be more than [Reserved] when
	// an estimate was low. Settle with zero used releases a request that
	// never reached upstream. A second Settle returns an error that wraps
	// [ErrReservationClosed]. Callers should pass a context that is not
	// tied to the client request, such as one from
	// [context.WithoutCancel], so that a lost client cannot stop the
	// settle.
	Settle(ctx context.Context, used int64) error
}
