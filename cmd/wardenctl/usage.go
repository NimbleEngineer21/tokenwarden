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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

func usage() command {
	var (
		team  string
		since time.Duration
	)
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "show only this team; empty = all teams")
			fs.DurationVar(&since, "since", 24*time.Hour, "show windows that ended less than this long ago, or are open")
		},
		check: func() error {
			if since <= 0 {
				return errors.New("-since must be positive")
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			rows, err := db.New(pool).ListUsage(ctx, db.ListUsageParams{Team: team, Since: time.Now().Add(-since)})
			if err != nil {
				return fmt.Errorf("list usage: %w", err)
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TEAM\tWINDOW START\tWINDOW\tSETTLED\tRESERVED\tBUDGET\tOPEN")
			for _, r := range rows {
				budget := "-"
				if r.BudgetTokens.Valid {
					budget = strconv.FormatInt(r.BudgetTokens.Int64, 10)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%d\n", r.Team,
					r.WindowStart.UTC().Format(time.RFC3339),
					formatWindow(time.Duration(r.WindowUs)*time.Microsecond),
					r.Settled, r.Reserved, budget, r.OpenReservations)
			}
			return tw.Flush()
		},
	}
}
