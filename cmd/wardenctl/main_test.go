package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/limit"
	"github.com/NimbleEngineer21/tokenwarden/limit/postgres"
)

func env(dbURL string) func(string) string {
	return func(name string) string {
		if name == "TW_DATABASE_URL" {
			return dbURL
		}
		return ""
	}
}

// A URL that would fail to connect: the usage errors must come first.
const noDB = "postgres://nobody@127.0.0.1:1/none"

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		dbURL string
		want  string
	}{
		{"no command", nil, noDB, "Usage:"},
		{"unknown command", []string{"keys"}, noDB, "Usage:"},
		{"group only", []string{"budgets"}, noDB, "Usage:"},
		{"no database", []string{"budgets", "list"}, "", "TW_DATABASE_URL is not set"},
		{"extra argument", []string{"budgets", "list", "x"}, noDB, `unexpected argument "x"`},
		{"bad flag", []string{"usage", "-nope"}, noDB, "flag provided but not defined"},
		{"set without team", []string{"budgets", "set", "-tokens", "1", "-window", "1h"}, noDB, "are required"},
		{"set without tokens", []string{"budgets", "set", "-team", "a", "-window", "1h"}, noDB, "are required"},
		{"set without window", []string{"budgets", "set", "-team", "a", "-tokens", "1"}, noDB, "are required"},
		{"delete without team", []string{"budgets", "delete"}, noDB, "-team is required"},
		{"bad max-inflight", []string{"budgets", "set", "-team", "a", "-tokens", "1", "-window", "1h", "-max-inflight", "x"}, noDB, "must be a whole number"},
		{"zero since", []string{"usage", "-since", "0s"}, noDB, "-since must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := run(t.Context(), tt.args, env(tt.dbURL), io.Discard, &stderr)
			if !errors.Is(err, errUsage) {
				t.Fatalf("run() = %v, want errUsage", err)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	var out bytes.Buffer
	if err := run(t.Context(), []string{"-h"}, env(""), &out, io.Discard); err != nil {
		t.Fatalf("run(-h) = %v", err)
	}
	if !strings.Contains(out.String(), "wardenctl budgets set") {
		t.Errorf("help = %q", out.String())
	}
	if err := run(t.Context(), []string{"usage", "-h"}, env(""), io.Discard, io.Discard); err != nil {
		t.Fatalf("run(usage -h) = %v", err)
	}
}

func TestBadURLHidesPassword(t *testing.T) {
	err := run(t.Context(), []string{"budgets", "list"}, env("postgres://u:tw_test_secret@[::1"), io.Discard, io.Discard)
	if err == nil || errors.Is(err, errUsage) || strings.Contains(err.Error(), "tw_test_secret") {
		t.Fatalf("err = %v, want a connect error without the password", err)
	}
}

func TestFormatWindow(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Hour:                   "1h",
		90 * time.Minute:            "1h30m",
		time.Minute:                 "1m",
		90 * time.Second:            "1m30s",
		1500 * time.Millisecond:     "1.5s",
		24*time.Hour + time.Second:  "24h0m1s",
		2*time.Hour + 3*time.Minute: "2h3m",
	} {
		if got := formatWindow(d); got != want {
			t.Errorf("formatWindow(%v) = %q, want %q", d, got, want)
		}
	}
}

// database returns the URL of a new, migrated schema and a pool on it.
func database(t *testing.T) (string, *pgxpool.Pool) {
	t.Helper()
	dbURL := pgtest.URL(t)
	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return dbURL, pool
}

// ctl runs wardenctl and returns its stdout. It fails the test on an error.
func ctl(t *testing.T, dbURL string, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if err := run(t.Context(), args, env(dbURL), &out, &stderr); err != nil {
		t.Fatalf("wardenctl %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out.String()
}

// fields splits output into lines of whitespace-separated fields.
func fields(out string) [][]string {
	var rows [][]string
	for line := range strings.Lines(out) {
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

func TestRefusesUnmigratedDatabase(t *testing.T) {
	err := run(t.Context(), []string{"budgets", "list"}, env(pgtest.URL(t)), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no tokenwarden schema") {
		t.Fatalf("err = %v, want no tokenwarden schema", err)
	}
}

func TestBudgets(t *testing.T) {
	dbURL, _ := database(t)
	if got := ctl(t, dbURL, "budgets", "set", "-team", "red", "-tokens", "1000", "-window", "1h"); got != "team red: 1000 tokens per 1h, no in-flight limit\n" {
		t.Errorf("set = %q", got)
	}
	if got := ctl(t, dbURL, "budgets", "set", "-team", "blue", "-tokens", "600", "-window", "1m", "-max-inflight", "3"); got != "team blue: 600 tokens per 1m, at most 3 in flight\n" {
		t.Errorf("set with cap = %q", got)
	}
	// A second set replaces the budget but keeps the cap when -max-inflight
	// is omitted. Team red stays first in the table, so the list must sort.
	if got := ctl(t, dbURL, "budgets", "set", "-team", "blue", "-tokens", "700", "-window", "90m"); got != "team blue: 700 tokens per 1h30m, at most 3 in flight\n" {
		t.Errorf("set without cap = %q", got)
	}
	header := []string{"TEAM", "TOKENS", "WINDOW", "MAX_INFLIGHT"}
	want := [][]string{header, {"blue", "700", "1h30m", "3"}, {"red", "1000", "1h", "0"}}
	if got := fields(ctl(t, dbURL, "budgets", "list")); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("list = %q, want %q", got, want)
	}
	// An explicit 0 clears the cap.
	ctl(t, dbURL, "budgets", "set", "-team", "blue", "-tokens", "700", "-window", "90m", "-max-inflight", "0")
	want = [][]string{header, {"blue", "700", "1h30m", "0"}, {"red", "1000", "1h", "0"}}
	if got := fields(ctl(t, dbURL, "budgets", "list")); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("list after clearing the cap = %q, want %q", got, want)
	}

	ctl(t, dbURL, "budgets", "delete", "-team", "blue")
	want = [][]string{header, {"red", "1000", "1h", "0"}}
	if got := fields(ctl(t, dbURL, "budgets", "list")); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("list after delete = %q, want %q", got, want)
	}
	err := run(t.Context(), []string{"budgets", "delete", "-team", "blue"}, env(dbURL), io.Discard, io.Discard)
	if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), "has no budget") {
		t.Errorf("second delete: err = %v, want has no budget", err)
	}
}

func TestBudgetsSetRejectsBadValues(t *testing.T) {
	dbURL, _ := database(t)
	for _, args := range [][]string{
		{"-team", "red", "-tokens", "-5", "-window", "1h"},
		{"-team", "red", "-tokens", "5", "-window", "-1h"},
		{"-team", "red", "-tokens", "5", "-window", "1ns"},
		{"-team", "red", "-tokens", "5", "-window", "1h", "-max-inflight", "-1"},
	} {
		err := run(t.Context(), append([]string{"budgets", "set"}, args...), env(dbURL), io.Discard, io.Discard)
		if err == nil || errors.Is(err, errUsage) {
			t.Errorf("set %v: err = %v, want a budget error", args, err)
		}
	}
	if got := fields(ctl(t, dbURL, "budgets", "list")); len(got) != 1 {
		t.Errorf("list = %q, want only the header", got)
	}
}

func TestUsage(t *testing.T) {
	dbURL, pool := database(t)
	ctx := t.Context()
	now := postgres.New(pool)
	if err := now.SetBudget(ctx, "red", limit.Budget{Tokens: 1000, Window: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if err := now.SetBudget(ctx, "blue", limit.Budget{Tokens: 600, Window: time.Hour}); err != nil {
		t.Fatal(err)
	}
	settled := reserve(ctx, t, now, "red", 100)
	if err := settled.Settle(ctx, 40); err != nil {
		t.Fatal(err)
	}
	reserve(ctx, t, now, "red", 70) // left open
	reserve(ctx, t, now, "blue", 5)

	// A window far in the past, with a window length that no budget has.
	old := postgres.New(pool, postgres.WithClock(func() time.Time {
		return time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	}))
	if err := old.SetBudget(ctx, "red", limit.Budget{Tokens: 50, Window: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if err := reserve(ctx, t, old, "red", 9).Settle(ctx, 9); err != nil {
		t.Fatal(err)
	}
	// An old window of blue with the current window length and an open
	// reservation: it must not count in the current window.
	reserve(ctx, t, old, "blue", 3)
	if err := now.SetBudget(ctx, "red", limit.Budget{Tokens: 1000, Window: time.Hour}); err != nil {
		t.Fatal(err)
	}

	start := limit.WindowOf(time.Now(), time.Hour).Start.UTC().Format(time.RFC3339)
	header := []string{"TEAM", "WINDOW", "START", "WINDOW", "SETTLED", "RESERVED", "BUDGET", "OPEN"}
	blue := []string{"blue", start, "1h", "0", "5", "600", "1"}
	red := []string{"red", start, "1h", "40", "70", "1000", "1"}
	redOld := []string{"red", "2001-02-03T04:05:00Z", "1m", "9", "0", "-", "0"}
	blueOld := []string{"blue", "2001-02-03T04:00:00Z", "1h", "0", "3", "600", "1"}
	tests := []struct {
		args []string
		want [][]string
	}{
		{nil, [][]string{header, blue, red}},
		{[]string{"-team", "red"}, [][]string{header, red}},
		{[]string{"-team", "red", "-since", "300000h"}, [][]string{header, red, redOld}},
		{[]string{"-since", "300000h"}, [][]string{header, blue, blueOld, red, redOld}},
		{[]string{"-team", "green"}, [][]string{header}},
	}
	for _, tt := range tests {
		got := fields(ctl(t, dbURL, append([]string{"usage"}, tt.args...)...))
		if !slices.EqualFunc(got, tt.want, slices.Equal) {
			t.Errorf("usage %v =\n%q\nwant\n%q", tt.args, got, tt.want)
		}
	}
}

func reserve(ctx context.Context, t *testing.T, l limit.Limiter, team string, tokens int64) limit.Reservation {
	t.Helper()
	r, err := l.Reserve(ctx, team, tokens)
	if err != nil {
		t.Fatalf("Reserve(%s, %d): %v", team, tokens, err)
	}
	return r
}

// TestDocsMatchFlags checks the flag tables of docs/wardenctl.md against the
// flag sets, names and defaults, so that a changed flag fails until the docs
// follow.
func TestDocsMatchFlags(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "wardenctl.md"))
	if err != nil {
		t.Fatal(err)
	}
	documented := map[string]string{}
	section := ""
	heading := regexp.MustCompile("^### `wardenctl ([a-z ]+)`")
	row := regexp.MustCompile("^\\| `-([a-z-]+)` \\| `?([^`|]*?)`? \\|")
	for line := range strings.Lines(string(doc)) {
		if m := heading.FindStringSubmatch(line); m != nil {
			section = m[1]
		} else if m := row.FindStringSubmatch(line); m != nil {
			documented[section+" -"+m[1]] = m[2]
		}
	}
	live := map[string]string{}
	for name, cmd := range commands() {
		fs := flagSetOf(cmd)
		fs.VisitAll(func(f *flag.Flag) {
			def := f.DefValue
			if f.DefValue == "" || f.DefValue == "0" || f.DefValue == "0s" {
				def = "none"
			}
			if d, err := time.ParseDuration(f.DefValue); err == nil && d > 0 {
				def = formatWindow(d)
			}
			live[name+" -"+f.Name] = def
		})
	}
	if !maps.Equal(documented, live) {
		t.Fatalf("flags in docs/wardenctl.md = %v, flag sets = %v", documented, live)
	}
}

func flagSetOf(cmd command) *flag.FlagSet {
	fs := flag.NewFlagSet("", flag.ContinueOnError)
	cmd.flags(fs)
	return fs
}
