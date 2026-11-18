package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

func usage() command {
	var (
		team   string
		since  time.Duration
		by     string
		hourly bool
		byKey  bool
		byMod  bool
	)
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "show only this team; empty = all teams")
			fs.DurationVar(&since, "since", 24*time.Hour, "show windows or hours that ended less than this long ago, or are open")
			fs.StringVar(&by, "by", "", "show the usage ledger per key, model, or key,model instead of the budget windows")
			fs.BoolVar(&hourly, "hourly", false, "show the usage ledger per hour instead of the budget windows")
		},
		check: func() error {
			if since <= 0 {
				return errors.New("-since must be positive")
			}
			if by != "" {
				for f := range strings.SplitSeq(by, ",") {
					switch f {
					case "key":
						byKey = true
					case "model":
						byMod = true
					default:
						return fmt.Errorf("-by %q: want key, model, or key,model", by)
					}
				}
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			from := time.Now().Add(-since)
			if by != "" || hourly {
				return listLedger(ctx, pool, stdout, db.ListLedgerParams{
					Team: team, Since: from, ByHour: hourly, ByKey: byKey, ByModel: byMod,
				})
			}
			rows, err := db.New(pool).ListUsage(ctx, db.ListUsageParams{Team: team, Since: from})
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

// listLedger prints the usage ledger sums that p selects, with a column for
// each grouping that p asks for.
func listLedger(ctx context.Context, pool *pgxpool.Pool, stdout io.Writer, p db.ListLedgerParams) error {
	rows, err := db.New(pool).ListLedger(ctx, p)
	if err != nil {
		return fmt.Errorf("list usage ledger: %w", err)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, ledgerColumns(p, "TEAM", "HOUR", "KEY", "MODEL")+
		"\tREQUESTS\tINPUT\tOUTPUT\tCACHE_WRITE\tCACHE_READ\tESTIMATED\tREAPED\tTOTAL")
	for _, r := range rows {
		total := int64(0)
		for _, n := range []int64{r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens, r.EstimatedTokens, r.ReapedTokens} {
			total = min(total, math.MaxInt64-n) + n
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			ledgerColumns(p, r.Team, r.Hour.UTC().Format(time.RFC3339), orDash(r.KeyID), orDash(r.Model)),
			r.Requests, r.InputTokens, r.OutputTokens, r.CacheWriteTokens, r.CacheReadTokens,
			r.EstimatedTokens, r.ReapedTokens, total)
	}
	return tw.Flush()
}

// ledgerColumns joins team with the hour, key, and model values that p
// groups by.
func ledgerColumns(p db.ListLedgerParams, team, hour, key, model string) string {
	cols := []string{team}
	if p.ByHour {
		cols = append(cols, hour)
	}
	if p.ByKey {
		cols = append(cols, key)
	}
	if p.ByModel {
		cols = append(cols, model)
	}
	return strings.Join(cols, "\t")
}
