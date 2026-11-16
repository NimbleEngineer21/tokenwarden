package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/limittest"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
)

// newPool returns a pool on a new, migrated schema.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.New(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func setBudgets(t *testing.T, l *postgres.Limiter, budgets map[string]limit.Budget) {
	t.Helper()
	for team, b := range budgets {
		if err := l.SetBudget(t.Context(), team, b); err != nil {
			t.Fatalf("SetBudget(%q): %v", team, err)
		}
	}
}

func TestConformance(t *testing.T) {
	limittest.Run(t, func(t *testing.T, budgets map[string]limit.Budget, now func() time.Time) limit.Limiter {
		t.Helper()
		l := postgres.New(newPool(t), postgres.WithClock(now))
		setBudgets(t, l, budgets)
		return l
	})
}

// TestReplicasShareBudget checks that two limiters with their own pools on
// one database see each other's reservations, as two gateway replicas do.
func TestReplicasShareBudget(t *testing.T) {
	pool := newPool(t)
	other, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(other.Close)

	now := func() time.Time { return time.Date(2001, 2, 3, 4, 5, 15, 0, time.UTC) }
	a := postgres.New(pool, postgres.WithClock(now))
	b := postgres.New(other, postgres.WithClock(now))
	setBudgets(t, a, map[string]limit.Budget{"team-a": {Tokens: 100, Window: time.Minute}})
	ctx := t.Context()

	ra, err := a.Reserve(ctx, "team-a", 70)
	if err != nil {
		t.Fatalf("replica a: Reserve: %v", err)
	}
	if _, err := b.Reserve(ctx, "team-a", 31); !errors.Is(err, limit.ErrBudgetExceeded) {
		t.Fatalf("replica b: Reserve(31): err = %v, want ErrBudgetExceeded", err)
	}
	rb, err := b.Reserve(ctx, "team-a", 30)
	if err != nil {
		t.Fatalf("replica b: Reserve(30): %v", err)
	}
	if err := ra.Settle(ctx, 10); err != nil {
		t.Fatalf("replica a: Settle: %v", err)
	}
	if err := rb.TopUp(ctx, 60); err != nil {
		t.Fatalf("replica b: TopUp after a settled: %v", err)
	}
	if err := rb.TopUp(ctx, 1); !errors.Is(err, limit.ErrBudgetExceeded) {
		t.Fatalf("replica b: TopUp(1) at budget: err = %v, want ErrBudgetExceeded", err)
	}
}

// TestSetBudgetKeepsOpenReservations checks that a lowered budget applies to
// new reservations at once and leaves open ones on the budget they started
// with.
func TestSetBudgetKeepsOpenReservations(t *testing.T) {
	now := func() time.Time { return time.Date(2001, 2, 3, 4, 5, 15, 0, time.UTC) }
	l := postgres.New(newPool(t), postgres.WithClock(now))
	setBudgets(t, l, map[string]limit.Budget{"team-a": {Tokens: 100, Window: time.Minute}})
	ctx := t.Context()

	r, err := l.Reserve(ctx, "team-a", 40)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	setBudgets(t, l, map[string]limit.Budget{"team-a": {Tokens: 50, Window: time.Minute}})
	if _, err := l.Reserve(ctx, "team-a", 11); !errors.Is(err, limit.ErrBudgetExceeded) {
		t.Fatalf("Reserve(11) under the new budget: err = %v, want ErrBudgetExceeded", err)
	}
	if err := r.TopUp(ctx, 60); err != nil {
		t.Fatalf("TopUp under the old budget: %v", err)
	}
}

func TestSetBudgetRejectsBadInput(t *testing.T) {
	l := postgres.New(newPool(t))
	tests := []struct {
		name string
		team string
		b    limit.Budget
	}{
		{"empty team", "", limit.Budget{Tokens: 1, Window: time.Minute}},
		{"zero tokens", "team-a", limit.Budget{Tokens: 0, Window: time.Minute}},
		{"negative tokens", "team-a", limit.Budget{Tokens: -1, Window: time.Minute}},
		{"zero window", "team-a", limit.Budget{Tokens: 1, Window: 0}},
		{"negative window", "team-a", limit.Budget{Tokens: 1, Window: -time.Minute}},
		{"sub-microsecond window", "team-a", limit.Budget{Tokens: 1, Window: time.Minute + time.Nanosecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := l.SetBudget(t.Context(), tt.team, tt.b); err == nil {
				t.Error("SetBudget returned no error")
			}
		})
	}
}
