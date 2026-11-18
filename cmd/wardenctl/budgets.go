package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
)

func budgetsSet() command {
	var (
		team        string
		tokens      int64
		window      time.Duration
		maxInflight optionalInt64
	)
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "team name (required)")
			fs.Int64Var(&tokens, "tokens", 0, "tokens per window (required)")
			fs.DurationVar(&window, "window", 0, "window length, for example 1h (required)")
			fs.Var(&maxInflight, "max-inflight", "most requests in flight at once; 0 means no limit (default: keep the current value)")
		},
		check: func() error {
			if team == "" || tokens == 0 || window == 0 {
				return errors.New("-team, -tokens, and -window are required")
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			b := limit.Budget{Tokens: tokens, Window: window, MaxInflight: maxInflight.v}
			if !maxInflight.set {
				// An omitted flag must not clear a cap. A concurrent change to
				// the cap between this read and the write is lost; two admins
				// that set one team at once race on every field anyway.
				cur, err := db.New(pool).GetBudget(ctx, team)
				switch {
				case err == nil:
					b.MaxInflight = cur.MaxInflight
				case !errors.Is(err, pgx.ErrNoRows):
					return fmt.Errorf("read budget: %w", err)
				}
			}
			// SetBudget checks the values, so the rules live in one place.
			if err := postgres.New(pool).SetBudget(ctx, team, b); err != nil {
				return err
			}
			_, err := fmt.Fprintf(stdout, "team %s: %d tokens per %s, %s\n", team, tokens, formatWindow(window), formatInflight(b.MaxInflight))
			return err
		},
	}
}

// optionalInt64 is an int64 flag that records whether the command line set it.
type optionalInt64 struct {
	v   int64
	set bool
}

func (o *optionalInt64) String() string {
	if o == nil || !o.set {
		return ""
	}
	return strconv.FormatInt(o.v, 10)
}

func (o *optionalInt64) Set(s string) error {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return errors.New("must be a whole number")
	}
	o.v, o.set = v, true
	return nil
}

func formatInflight(n int64) string {
	if n == 0 {
		return "no in-flight limit"
	}
	return fmt.Sprintf("at most %d in flight", n)
}

func budgetsList() command {
	return command{
		flags: func(*flag.FlagSet) {},
		check: func() error { return nil },
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			budgets, err := db.New(pool).ListBudgets(ctx)
			if err != nil {
				return fmt.Errorf("list budgets: %w", err)
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TEAM\tTOKENS\tWINDOW\tMAX_INFLIGHT")
			for _, b := range budgets {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%d\n", b.Team, b.Tokens, formatWindow(time.Duration(b.WindowUs)*time.Microsecond), b.MaxInflight)
			}
			return tw.Flush()
		},
	}
}

func budgetsDelete() command {
	var team string
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "team name (required)")
		},
		check: func() error {
			if team == "" {
				return errors.New("-team is required")
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			n, err := db.New(pool).DeleteBudget(ctx, team)
			if err != nil {
				return fmt.Errorf("delete budget: %w", err)
			}
			if n == 0 {
				return fmt.Errorf("team %s has no budget", team)
			}
			_, err = fmt.Fprintf(stdout, "team %s: budget deleted; its requests now get 403\n", team)
			return err
		},
	}
}
