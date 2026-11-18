// Command wardenctl is the admin tool of tokenwarden. It sets and lists the
// token budgets of teams, shows their token usage, and creates, lists, and
// revokes gateway keys.
//
// The database URL comes from the environment variable TW_DATABASE_URL,
// never from a flag, so that the password does not show in the process list
// or in shell history. wardenctl never changes the schema: it refuses to run
// when the database schema is not the one that this build knows. See
// docs/wardenctl.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/store"
)

// errUsage means that the command line was wrong and the error is already
// printed.
var errUsage = errors.New("usage error")

// connectTimeout bounds the database connect and the schema check.
const connectTimeout = 10 * time.Second

const usageText = `Usage:
  wardenctl budgets set -team TEAM -tokens N -window DURATION
  wardenctl budgets list
  wardenctl budgets delete -team TEAM
  wardenctl usage [-team TEAM] [-since DURATION] [-by key,model] [-hourly]
  wardenctl keys create -team TEAM [-user USER]
  wardenctl keys list [-team TEAM] [-all]
  wardenctl keys revoke -id KEYID

The database URL comes from TW_DATABASE_URL. See docs/wardenctl.md.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	switch {
	case errors.Is(err, errUsage):
		os.Exit(2)
	case err != nil:
		fmt.Fprintln(os.Stderr, "wardenctl:", err)
		os.Exit(1)
	}
}

// command is one subcommand: its flags, a check of the parsed flags, and
// the action against the database.
type command struct {
	flags func(fs *flag.FlagSet)
	check func() error
	do    func(ctx context.Context, pool *pgxpool.Pool, stdout, stderr io.Writer) error
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	name, cmd, rest, ok := lookup(args)
	if !ok {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "help") {
			_, err := io.WriteString(stdout, usageText)
			return err
		}
		fmt.Fprint(stderr, usageText)
		return errUsage
	}
	fs := flag.NewFlagSet("wardenctl "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cmd.flags(fs)
	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	var err error
	switch {
	case fs.NArg() > 0:
		err = fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case getenv("TW_DATABASE_URL") == "":
		err = errors.New("TW_DATABASE_URL is not set")
	default:
		err = cmd.check()
	}
	if err != nil {
		fmt.Fprintf(stderr, "wardenctl %s: %v\n", name, err)
		return errUsage
	}
	pool, err := connect(ctx, getenv("TW_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	return cmd.do(ctx, pool, stdout, stderr)
}

// lookup finds the subcommand that args name and returns its full name and
// the arguments after it.
func lookup(args []string) (string, command, []string, bool) {
	cmds := commands()
	for n := 1; n <= min(2, len(args)); n++ {
		name := strings.Join(args[:n], " ")
		if cmd, ok := cmds[name]; ok {
			return name, cmd, args[n:], true
		}
	}
	return "", command{}, nil, false
}

func commands() map[string]command {
	return map[string]command{
		"budgets set":    budgetsSet(),
		"budgets list":   budgetsList(),
		"budgets delete": budgetsDelete(),
		"usage":          usage(),
		"keys create":    keysCreate(),
		"keys list":      keysList(),
		"keys revoke":    keysRevoke(),
	}
}

// connect opens a pool and checks that the schema matches this build.
func connect(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		// The parse error can quote the URL, password included.
		return nil, errors.New("TW_DATABASE_URL is not a valid Postgres URL")
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := store.CheckVersion(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// formatWindow prints a window length without zero tails: 1h, not 1h0m0s.
func formatWindow(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
