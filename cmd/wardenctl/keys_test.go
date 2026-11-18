package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	authpg "github.com/NimbleEngineer21/tokenwarden/auth/postgres"
	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

// defaultNewKey is the real key generator, which TestKeysCreateClash puts
// back.
var defaultNewKey = newKey

func TestKeyIDPatternMatchesAuth(t *testing.T) {
	id := auth.KeyID(auth.HashKey("tw_test_any"))
	if !keyIDPattern.MatchString(id) {
		t.Fatalf("keyIDPattern does not match auth.KeyID %q", id)
	}
}

func TestNewKey(t *testing.T) {
	a, b := newKey(), newKey()
	if a == b {
		t.Fatal("two new keys are equal")
	}
	rest, ok := strings.CutPrefix(a, "tw_")
	if !ok {
		t.Fatalf("key %q has no tw_ prefix", a)
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) != 32 {
		t.Fatalf("key body: %d bytes, err %v; want 32 bytes of base64url", len(raw), err)
	}
	// One key has no + or / with a chance of about 1 in 4; 64 keys catch a
	// switch to standard base64 with near certainty.
	form := regexp.MustCompile(`^tw_[A-Za-z0-9_-]{43}$`)
	for range 64 {
		if k := newKey(); !form.MatchString(k) {
			t.Fatalf("key %q is not tw_ and 43 base64url characters", k)
		}
	}
}

// ctlErr runs wardenctl and returns its stdout, its stderr, and its error.
func ctlErr(t *testing.T, dbURL string, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(t.Context(), args, env(dbURL), &out, &stderr)
	return out.String(), stderr.String(), err
}

// insertKeyRow adds a key row with a fixed hash, for the clash cases.
func insertKeyRow(t *testing.T, pool *pgxpool.Pool, hash, team string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), "INSERT INTO gateway_keys (sha256, team) VALUES ($1, $2)", hash, team); err != nil {
		t.Fatal(err)
	}
}

func TestKeys(t *testing.T) {
	dbURL, pool := database(t)
	authn := authpg.New(pool)
	ctl(t, dbURL, "budgets", "set", "-team", "red", "-tokens", "1000", "-window", "1h")

	out, stderr, err := ctlErr(t, dbURL, "keys", "create", "-team", "red", "-user", "alice")
	if err != nil {
		t.Fatalf("keys create: %v\n%s", err, stderr)
	}
	key := strings.TrimSuffix(out, "\n")
	if strings.ContainsAny(key, " \n") || !strings.HasPrefix(key, "tw_") {
		t.Fatalf("stdout = %q, want the key alone", out)
	}
	id := auth.KeyID(auth.HashKey(key))
	if !strings.Contains(stderr, "key ID "+id) || !strings.Contains(stderr, "not shown again") {
		t.Errorf("stderr = %q, want the key ID and the note", stderr)
	}
	if strings.Contains(stderr, key) || strings.Contains(stderr, auth.HashKey(key)) || strings.Contains(stderr, "warning") {
		t.Errorf("stderr = %q: has the key, its hash, or a warning for a team with a budget", stderr)
	}
	got, err := authn.Authenticate(t.Context(), key)
	if err != nil || got != (auth.Identity{KeyID: id, Team: "red", User: "alice"}) {
		t.Fatalf("Authenticate(new key) = %+v, %v", got, err)
	}

	_, stderr, err = ctlErr(t, dbURL, "keys", "create", "-team", "green")
	if err != nil || !strings.Contains(stderr, "warning: team green has no budget") {
		t.Errorf("keys create for a team without a budget: err %v, stderr %q; want a warning", err, stderr)
	}

	rows := fields(ctl(t, dbURL, "keys", "list"))
	if len(rows) != 3 || strings.Join(rows[0], " ") != "KEY ID TEAM USER CREATED REVOKED" {
		t.Fatalf("keys list = %v", rows)
	}
	green := rows[1]
	if green[1] != "green" || green[2] != "-" || green[4] != "-" {
		t.Errorf("green row = %v, want user - and revoked -", green)
	}
	if got := ctl(t, dbURL, "keys", "revoke", "-id", green[0]); got != "key "+green[0]+" of team green revoked; its requests now get 401\n" {
		t.Errorf("keys revoke of the green key = %q", got)
	}
	if red := rows[2]; red[0] != id || red[1] != "red" || red[2] != "alice" {
		t.Errorf("red row = %v, want key ID %s, red, alice", red, id)
	}
	if rows := fields(ctl(t, dbURL, "keys", "list", "-team", "red")); len(rows) != 2 || rows[1][0] != id {
		t.Errorf("keys list -team red = %v", rows)
	}

	if got := ctl(t, dbURL, "keys", "revoke", "-id", id); got != "key "+id+" of team red revoked; its requests now get 401\n" {
		t.Errorf("keys revoke = %q", got)
	}
	if _, err := authn.Authenticate(t.Context(), key); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Authenticate(revoked key) err = %v, want ErrUnauthorized", err)
	}
	if rows := fields(ctl(t, dbURL, "keys", "list", "-team", "red")); len(rows) != 1 {
		t.Errorf("keys list -team red after revoke = %v, want the header only", rows)
	}
	rows = fields(ctl(t, dbURL, "keys", "list", "-team", "red", "-all"))
	if len(rows) != 2 || rows[1][0] != id || rows[1][4] == "-" {
		t.Errorf("keys list -all = %v, want the red key with a revoke time", rows)
	}
	if _, _, err := ctlErr(t, dbURL, "keys", "revoke", "-id", id); err == nil || !strings.Contains(err.Error(), "no active key") {
		t.Errorf("second revoke err = %v, want no active key", err)
	}
}

// failWriter fails every write, as a full disk does.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestKeysCreateRevokesUnseenKey(t *testing.T) {
	dbURL, pool := database(t)
	err := run(t.Context(), []string{"keys", "create", "-team", "red"}, env(dbURL), failWriter{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "disk full") || !strings.Contains(err.Error(), "is revoked") {
		t.Fatalf("keys create with a failed stdout: err = %v", err)
	}
	if n, err := authpg.New(pool).Count(t.Context()); err != nil || n != 0 {
		t.Errorf("active keys = %d, %v; want 0", n, err)
	}
	if rows := fields(ctl(t, dbURL, "keys", "list", "-all")); len(rows) != 2 || rows[1][4] == "-" {
		t.Errorf("keys list -all = %v, want one revoked key", rows)
	}
}

// budgetFunc is a budgetReader from a function.
type budgetFunc func() (db.GetBudgetRow, error)

func (f budgetFunc) GetBudget(context.Context, string) (db.GetBudgetRow, error) { return f() }

func TestWarnNoBudget(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"budget", nil, ""},
		{"no budget", pgx.ErrNoRows, "warning: team red has no budget"},
		{"read fails", errors.New("connection reset"), "warning: could not check the budget of team red: connection reset"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			warnNoBudget(t.Context(), budgetFunc(func() (db.GetBudgetRow, error) { return db.GetBudgetRow{}, tt.err }), "red", &stderr)
			if tt.want == "" && stderr.Len() > 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestKeysCreateClash(t *testing.T) {
	dbURL, pool := database(t)
	authn := authpg.New(pool)
	clash, fresh := "tw_test_clash", "tw_test_fresh"
	// A revoked row with the key ID of the first key still blocks that ID.
	hash := auth.HashKey(clash)
	insertKeyRow(t, pool, auth.KeyID(hash)+strings.Repeat("0", 52), "blue")
	if _, err := pool.Exec(t.Context(), "UPDATE gateway_keys SET revoked_at = now()"); err != nil {
		t.Fatal(err)
	}

	keys := []string{clash, fresh}
	newKey = func() string {
		k := keys[0]
		keys = keys[1:]
		return k
	}
	defer func() { newKey = defaultNewKey }()
	out, _, err := ctlErr(t, dbURL, "keys", "create", "-team", "red")
	if err != nil || out != fresh+"\n" {
		t.Fatalf("keys create = %q, %v; want the second key", out, err)
	}
	if _, err := authn.Authenticate(t.Context(), clash); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("Authenticate(clashing key) err = %v, want ErrUnauthorized", err)
	}

	newKey = func() string { return clash }
	_, _, err = ctlErr(t, dbURL, "keys", "create", "-team", "red")
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("keys create with only clashing keys: err = %v", err)
	}
	if n, err := authn.Count(t.Context()); err != nil || n != 1 {
		t.Errorf("active keys = %d, %v; want 1", n, err)
	}
}
