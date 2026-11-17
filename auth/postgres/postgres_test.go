package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/auth/postgres"
	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := pgtest.New(t)
	if err := postgres.Migrate(t.Context(), pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return pool
}

func addKey(t *testing.T, pool *pgxpool.Pool, key, team, user string) {
	t.Helper()
	_, err := pool.Exec(t.Context(),
		"INSERT INTO gateway_keys (sha256, team, user_name) VALUES ($1, $2, $3)",
		auth.HashKey(key), team, user)
	if err != nil {
		t.Fatalf("add key: %v", err)
	}
}

func TestAuthenticate(t *testing.T) {
	pool := newPool(t)
	ctx := t.Context()
	addKey(t, pool, "tw_test_red_alice", "red", "alice")
	addKey(t, pool, "tw_test_blue_service", "blue", "")
	a := postgres.New(pool)

	for _, tc := range []struct {
		key  string
		want auth.Identity
	}{
		{"tw_test_red_alice", auth.Identity{KeyID: auth.KeyID(auth.HashKey("tw_test_red_alice")), Team: "red", User: "alice"}},
		{"tw_test_blue_service", auth.Identity{KeyID: auth.KeyID(auth.HashKey("tw_test_blue_service")), Team: "blue"}},
	} {
		got, err := a.Authenticate(ctx, tc.key)
		if err != nil || got != tc.want {
			t.Errorf("Authenticate(%s) = %+v, %v; want %+v", tc.key, got, err, tc.want)
		}
	}
	if n, err := a.Count(ctx); err != nil || n != 2 {
		t.Errorf("Count = %d, %v; want 2", n, err)
	}

	if _, err := a.Authenticate(ctx, "tw_test_unknown"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("unknown key: err = %v, want ErrUnauthorized", err)
	}

	// A revoked key fails on the next request: there is no cache.
	if _, err := pool.Exec(ctx, "UPDATE gateway_keys SET revoked_at = now() WHERE sha256 = $1", auth.HashKey("tw_test_red_alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Authenticate(ctx, "tw_test_red_alice"); !errors.Is(err, auth.ErrUnauthorized) {
		t.Errorf("revoked key: err = %v, want ErrUnauthorized", err)
	}
	if n, err := a.Count(ctx); err != nil || n != 1 {
		t.Errorf("Count after revoke = %d, %v; want 1", n, err)
	}
}

func TestAuthenticateDatabaseDown(t *testing.T) {
	pool := newPool(t)
	a := postgres.New(pool)
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	const key = "tw_test_db_down"
	_, err := a.Authenticate(ctx, key)
	if err == nil || errors.Is(err, auth.ErrUnauthorized) {
		t.Fatalf("err = %v, want a database error", err)
	}
	if msg := err.Error(); strings.Contains(msg, key) || strings.Contains(msg, auth.HashKey(key)) {
		t.Errorf("error %q holds the key or its hash", msg)
	}
	if _, err := a.Count(ctx); err == nil {
		t.Error("Count: err = nil, want a database error")
	}
}

func TestTableRefusesBadRows(t *testing.T) {
	pool := newPool(t)
	for name, args := range map[string][]any{
		"short hash":     {"abc", "red"},
		"uppercase hash": {strings.ToUpper(auth.HashKey("tw_test_x")), "red"},
		"empty team":     {auth.HashKey("tw_test_x"), ""},
	} {
		if _, err := pool.Exec(t.Context(), "INSERT INTO gateway_keys (sha256, team) VALUES ($1, $2)", args...); err == nil {
			t.Errorf("%s: insert succeeded, want a check violation", name)
		}
	}
}
