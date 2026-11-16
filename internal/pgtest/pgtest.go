// Package pgtest gives a test its own empty Postgres schema.
//
// Tests that need Postgres read the server address from the
// TW_TEST_DATABASE_URL environment variable. Without it they skip, except
// under CI=true, where they fail so that a missing database service cannot
// hide them. `make test-pg` starts a local server and sets the variable.
package pgtest

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvURL names the environment variable that holds the test server URL.
const EnvURL = "TW_TEST_DATABASE_URL"

// New creates a schema with a random name, returns a pool whose connections
// use only that schema, and drops the schema when the test ends.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), URL(t))
	if err != nil {
		t.Fatalf("pgtest: open pool: %v", err)
	}
	// Registered after the schema cleanup, so it runs first: open
	// connections must close before the schema drops.
	t.Cleanup(pool.Close)
	return pool
}

// URL creates a schema with a random name and returns a connection URL
// whose connections use only that schema. The schema drops when the test
// ends; close every connection to it before then.
func URL(t *testing.T) string {
	t.Helper()
	raw := os.Getenv(EnvURL)
	if raw == "" {
		if os.Getenv("CI") == "true" {
			t.Fatalf("%s is not set; CI must run the Postgres tests", EnvURL)
		}
		t.Skipf("%s is not set; run make test-pg", EnvURL)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("pgtest: %s must be a postgres:// URL", EnvURL)
	}
	ctx := t.Context()
	schema := "tw_test_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()

	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		_ = admin.Close(ctx)
		t.Fatalf("pgtest: create schema: %v", err)
	}
	t.Cleanup(func() {
		// t.Context is canceled before cleanups run.
		ctx := context.Background()
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("pgtest: drop schema: %v", err)
		}
		_ = admin.Close(ctx)
	})

	// pgx sends an unknown URL parameter to the server as a run-time
	// setting.
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
