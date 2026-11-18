package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NimbleEngineer21/tokenwarden/auth"
	"github.com/NimbleEngineer21/tokenwarden/internal/store/db"
)

// keyPrefix starts every key that wardenctl makes, so that a secret scanner
// can find a leaked key.
const keyPrefix = "tw_"

// keyBytes is the random part of a key: 256 bits.
const keyBytes = 32

// createTries bounds the attempts to find a key whose key ID no other row
// has (the unique index gateway_keys_key_id). With 48-bit key IDs, a second
// attempt is already very improbable.
const createTries = 3

// keyIDPattern is the form of a key ID from [auth.KeyID]. A test keeps the
// length in step with the auth package.
var keyIDPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// newKey returns a new random gateway key. It is a variable so that a test
// can force a key ID clash.
var newKey = func() string {
	b := make([]byte, keyBytes)
	// crypto/rand.Read never returns an error; it crashes the program
	// instead.
	_, _ = rand.Read(b)
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func keysCreate() command {
	var team, user string
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "team that owns the key (required)")
			fs.StringVar(&user, "user", "", "person or service that holds the key")
		},
		check: func() error {
			if team == "" {
				return errors.New("-team is required")
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, stderr io.Writer) error {
			q := db.New(pool)
			for range createTries {
				key := newKey()
				hash := auth.HashKey(key)
				n, err := q.InsertKey(ctx, db.InsertKeyParams{Sha256: hash, Team: team, UserName: user})
				if err != nil {
					return fmt.Errorf("create key: %w", err)
				}
				if n == 0 {
					continue
				}
				// The key goes to stdout alone, so that a script can capture
				// it; everything else goes to stderr.
				if _, err := fmt.Fprintln(stdout, key); err != nil {
					return revokeUnseen(ctx, q, auth.KeyID(hash), err)
				}
				fmt.Fprintf(stderr, "key ID %s, team %s: store the key now; it is not shown again\n", auth.KeyID(hash), team)
				warnNoBudget(ctx, q, team, stderr)
				return nil
			}
			return fmt.Errorf("create key: %d new keys had a key ID that is already in use", createTries)
		},
	}
}

// revokeTimeout bounds the revoke of a key that could not be shown.
const revokeTimeout = 5 * time.Second

// revokeUnseen revokes a new key whose write to stdout failed: nobody holds
// it, so it must not stay active. It runs even when ctx is canceled, so that
// an interrupt does not leave the key active.
func revokeUnseen(ctx context.Context, q *db.Queries, id string, writeErr error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), revokeTimeout)
	defer cancel()
	if _, err := q.RevokeKey(ctx, id); err != nil {
		return fmt.Errorf("write key: %w; revoke the unseen key %s: %w", writeErr, id, err)
	}
	return fmt.Errorf("write key: %w; the key %s is revoked", writeErr, id)
}

// budgetReader reads the budget of a team. *db.Queries implements it; a
// test fakes a failed read.
type budgetReader interface {
	GetBudget(ctx context.Context, team string) (db.GetBudgetRow, error)
}

// warnNoBudget tells the admin that the key of a team without a budget gets
// 403 until the team has one. The key is valid and already printed, so a
// failed check is a note, not an error: an error exit would make a script
// drop a key that works.
func warnNoBudget(ctx context.Context, q budgetReader, team string, stderr io.Writer) {
	_, err := q.GetBudget(ctx, team)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		fmt.Fprintf(stderr, "warning: team %s has no budget; its requests get 403 until it has one\n", team)
	case err != nil:
		fmt.Fprintf(stderr, "warning: could not check the budget of team %s: %v\n", team, err)
	}
}

func keysList() command {
	var (
		team string
		all  bool
	)
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&team, "team", "", "show only this team; empty = all teams")
			fs.BoolVar(&all, "all", false, "also show revoked keys")
		},
		check: func() error { return nil },
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			keys, err := db.New(pool).ListKeys(ctx, db.ListKeysParams{Team: team, AllKeys: all})
			if err != nil {
				return fmt.Errorf("list keys: %w", err)
			}
			tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "KEY ID\tTEAM\tUSER\tCREATED\tREVOKED")
			for _, k := range keys {
				revoked := "-"
				if k.RevokedAt.Valid {
					revoked = k.RevokedAt.Time.UTC().Format(time.RFC3339)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.KeyID, k.Team, orDash(k.UserName),
					k.CreatedAt.UTC().Format(time.RFC3339), revoked)
			}
			return tw.Flush()
		},
	}
}

// orDash keeps an empty column visible, so that the fields of a line stay
// in place for a script.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func keysRevoke() command {
	var id string
	return command{
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&id, "id", "", "key ID from keys list (required)")
		},
		check: func() error {
			if !keyIDPattern.MatchString(id) {
				return errors.New("-id must be a key ID: 12 lowercase hex characters")
			}
			return nil
		},
		do: func(ctx context.Context, pool *pgxpool.Pool, stdout, _ io.Writer) error {
			team, err := db.New(pool).RevokeKey(ctx, id)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return fmt.Errorf("no active key has the key ID %s", id)
			case err != nil:
				return fmt.Errorf("revoke key: %w", err)
			}
			_, err = fmt.Fprintf(stdout, "key %s of team %s revoked; its requests now get 401\n", id, team)
			return err
		},
	}
}
