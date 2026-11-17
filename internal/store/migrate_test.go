package store_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/NimbleEngineer21/tokenwarden/internal/pgtest"
	"github.com/NimbleEngineer21/tokenwarden/internal/store"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	for i := range 2 {
		if err := store.Migrate(ctx, pool); err != nil {
			t.Fatalf("Migrate #%d: %v", i+1, err)
		}
	}
	rows, err := pool.Query(ctx, "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != 1 {
		t.Fatalf("versions = %v, want to start with 1", got)
	}
	for _, table := range []string{"budgets", "usage_windows", "reservations"} {
		var n int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestMigrateConcurrently(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range cap(errs) {
		wg.Go(func() { errs <- store.Migrate(ctx, pool) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Migrate: %v", err)
		}
	}
	var n, distinct int
	if err := pool.QueryRow(ctx, "SELECT count(*), count(DISTINCT version) FROM schema_migrations").Scan(&n, &distinct); err != nil {
		t.Fatal(err)
	}
	if n != distinct {
		t.Errorf("schema_migrations has %d rows for %d versions", n, distinct)
	}
}

func TestMigrateRejectsNewerDatabase(t *testing.T) {
	pool := pgtest.New(t)
	ctx := t.Context()
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES (999999)"); err != nil {
		t.Fatal(err)
	}
	err := store.Migrate(ctx, pool)
	if err == nil || !strings.Contains(err.Error(), "unknown version 999999") {
		t.Fatalf("err = %v, want unknown version 999999", err)
	}
}

func TestCheckVersion(t *testing.T) {
	tests := []struct {
		name    string
		migrate bool
		change  string
		want    string
	}{
		{"empty database", false, "", "no tokenwarden schema"},
		{"current", true, "", ""},
		{"older database", true, "DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)", "lacks migration"},
		{"newer database", true, "INSERT INTO schema_migrations (version) VALUES (999999)", "version 999999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := pgtest.New(t)
			ctx := t.Context()
			if tt.migrate {
				if err := store.Migrate(ctx, pool); err != nil {
					t.Fatalf("Migrate: %v", err)
				}
			}
			if tt.change != "" {
				if _, err := pool.Exec(ctx, tt.change); err != nil {
					t.Fatal(err)
				}
			}
			err := store.CheckVersion(ctx, pool)
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
