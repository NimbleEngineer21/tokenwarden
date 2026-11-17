// Package postgres is an [auth.Authenticator] that reads gateway keys from
// Postgres, so that replicas share them and an admin can add or revoke a
// key without a restart.
//
// The table holds the SHA-256 hash of each key, never the key. Each
// [Authenticator.Authenticate] call is one primary-key SELECT, with no
// cache, so a revoked key fails from the next request on.
//
// The database must have the tokenwarden schema; [Migrate] creates it.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/store"
	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

// Migrate creates or updates the tokenwarden schema in the database of
// pool. It is the same schema as the one of the budget limiter, so one call
// serves both.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return store.Migrate(ctx, pool)
}

// Authenticator is an [auth.Authenticator] backed by the gateway_keys
// table. It is safe for concurrent use.
type Authenticator struct {
	q *db.Queries
}

// New returns an Authenticator that reads keys from the database of pool.
func New(pool *pgxpool.Pool) *Authenticator {
	return &Authenticator{q: db.New(pool)}
}

// Authenticate implements [auth.Authenticator]. A revoked key is unknown.
// An error other than [auth.ErrUnauthorized] means the database failed;
// it never holds the key or its hash.
func (a *Authenticator) Authenticate(ctx context.Context, key string) (auth.Identity, error) {
	hash := auth.HashKey(key)
	row, err := a.q.GetActiveKey(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Identity{}, auth.ErrUnauthorized
	}
	if err != nil {
		return auth.Identity{}, fmt.Errorf("auth/postgres: look up key: %w", err)
	}
	return auth.Identity{KeyID: auth.KeyID(hash), Team: row.Team, User: row.UserName}, nil
}

// Count returns the number of keys that are not revoked.
func (a *Authenticator) Count(ctx context.Context) (int64, error) {
	n, err := a.q.CountActiveKeys(ctx)
	if err != nil {
		return 0, fmt.Errorf("auth/postgres: count keys: %w", err)
	}
	return n, nil
}
