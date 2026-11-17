// Package store holds the Postgres schema of tokenwarden and applies it.
//
// The schema lives in embedded, numbered SQL files under migrations/. The
// queries under queries/ are compiled by sqlc into package
// [github.com/NimbleEngineer21/tokenwarden/internal/store/db]; run
// `make generate` after a change to either.
package store

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// lockKey is the Postgres advisory lock that serializes migrations. Its value
// is arbitrary; it only has to differ from other advisory locks in the same
// database.
const lockKey int64 = 0x746f6b656e77 // "tokenw"

// Beginner starts a transaction. A *pgxpool.Pool and a [*pgx.Conn]
// implement it.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

type migration struct {
	version int64
	name    string
	sql     string
}

// Migrate applies every migration that the database does not have yet, in
// version order, in one transaction. Replicas that start at the same time
// are safe: an advisory lock makes them wait for each other, and the later
// ones find nothing to do. Migrate returns an error if the database has a
// version that this binary does not know, which means that a newer release
// already migrated it.
func Migrate(ctx context.Context, db Beginner) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockKey); err != nil {
		return fmt.Errorf("store: migrate: lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    bigint      PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("store: migrate: create schema_migrations: %w", err)
	}
	rows, err := tx.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("store: migrate: read versions: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("store: migrate: read versions: %w", err)
	}
	for _, v := range applied {
		if !slices.ContainsFunc(ms, func(m migration) bool { return m.version == v }) {
			return fmt.Errorf("store: migrate: database has unknown version %d; it was migrated by a newer release", v)
		}
	}
	for _, m := range ms {
		if slices.Contains(applied, m.version) {
			continue
		}
		if _, err := tx.Exec(ctx, m.sql); err != nil {
			return fmt.Errorf("store: migrate: %s: %w", m.name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", m.version); err != nil {
			return fmt.Errorf("store: migrate: %s: record version: %w", m.name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: migrate: commit: %w", err)
	}
	return nil
}

// Querier runs a query. A *pgxpool.Pool and a [*pgx.Conn] implement it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// CheckVersion returns nil if the database has exactly the migrations that
// this build embeds. Tools that must not change the schema call it instead
// of [Migrate]. The error says which side to upgrade.
func CheckVersion(ctx context.Context, db Querier) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	// The table check runs first: a query on a missing table fails at
	// parse time, with an error that does not say what to do.
	rows, err := db.Query(ctx, "SELECT to_regclass('schema_migrations') IS NOT NULL")
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	exists, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[bool])
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if !exists {
		return errors.New("store: the database has no tokenwarden schema; start tokenwarden once to create it")
	}
	rows, err = db.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	applied, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	for _, v := range applied {
		if !slices.ContainsFunc(ms, func(m migration) bool { return m.version == v }) {
			return fmt.Errorf("store: the database has schema version %d, which this build does not know; upgrade this tool", v)
		}
	}
	for _, m := range ms {
		if !slices.Contains(applied, m.version) {
			return fmt.Errorf("store: the database lacks migration %s; upgrade and start tokenwarden first", m.name)
		}
	}
	return nil
}

// migrations returns the embedded migrations sorted by version. A file name
// is a positive version number, an underscore, a name, and ".sql".
func migrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	ms := make([]migration, 0, len(names))
	for _, path := range names {
		name := strings.TrimPrefix(path, "migrations/")
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %s: name must start with a positive version and an underscore", name)
		}
		b, err := migrationFiles.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", name, err)
		}
		ms = append(ms, migration{version: v, name: name, sql: string(b)})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("store: migrations %s and %s have the same version", ms[i-1].name, ms[i].name)
		}
	}
	if len(ms) == 0 {
		return nil, errors.New("store: no migrations embedded")
	}
	return ms, nil
}
