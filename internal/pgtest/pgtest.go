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
	url := os.Getenv(EnvURL)
	if url == "" {
		if os.Getenv("CI") == "true" {
			t.Fatalf("%s is not set; CI must run the Postgres tests", EnvURL)
		}
		t.Skipf("%s is not set; run make test-pg", EnvURL)
	}
	ctx := t.Context()
	schema := "tw_test_" + strings.ToLower(rand.Text())
	ident := pgx.Identifier{schema}.Sanitize()

	admin, err := pgx.Connect(ctx, url)
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

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("pgtest: parse %s: %v", EnvURL, err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgtest: open pool: %v", err)
	}
	// Registered after the schema cleanup, so it runs first: open
	// connections must close before the schema drops.
	t.Cleanup(pool.Close)
	return pool
}
