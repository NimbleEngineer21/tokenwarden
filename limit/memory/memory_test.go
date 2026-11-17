package memory_test

import (
	"testing"
	"time"

	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/limittest"
	"github.com/NimbleEngineer21/tokenwarden/limit/memory"
)

func TestConformance(t *testing.T) {
	limittest.Run(t, func(t *testing.T, budgets map[string]limit.Budget, now func() time.Time) limit.Limiter {
		t.Helper()
		l, err := memory.New(budgets, memory.WithClock(now))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return l
	})
}

func TestNewRejectsBadBudgets(t *testing.T) {
	tests := []struct {
		name    string
		budgets map[string]limit.Budget
	}{
		{"empty team", map[string]limit.Budget{"": {Tokens: 1, Window: time.Minute}}},
		{"zero tokens", map[string]limit.Budget{"team-a": {Tokens: 0, Window: time.Minute}}},
		{"negative tokens", map[string]limit.Budget{"team-a": {Tokens: -1, Window: time.Minute}}},
		{"zero window", map[string]limit.Budget{"team-a": {Tokens: 1, Window: 0}}},
		{"negative window", map[string]limit.Budget{"team-a": {Tokens: 1, Window: -time.Minute}}},
		{"negative max in-flight", map[string]limit.Budget{"team-a": {Tokens: 1, Window: time.Minute, MaxInflight: -1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := memory.New(tt.budgets); err == nil {
				t.Error("New returned no error")
			}
		})
	}
}
