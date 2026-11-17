# 0002. Fail closed when the budget is unknown

Status: accepted
Date: 2026-11-17

## Context

The gateway can fail to learn a team's budget or to charge it in these
cases:

- The team has no budget row.
- The database is not reachable, or a query fails.
- A replica runs code from before a migration, against a schema from after
  it, during a rolling upgrade.

The gateway can answer each case in one of two ways. It can forward the
request, so the team keeps working but the budget is not enforced (fail
open). It can refuse the request (fail closed). A governance gateway that
fails open lets any outage, or any new team without a row, spend without
limit.

## Decision

The gateway fails closed in every case above. The gateway does not have a
fail-open option.

- **No budget.** `Reserve` returns an error that wraps `limit.ErrNoBudget`
  ([`limit/postgres/postgres.go`](../../limit/postgres/postgres.go) and
  [`limit/memory/memory.go`](../../limit/memory/memory.go)). The gateway
  replies 403 `permission_error` and does not call the upstream
  ([`gateway/budget.go`](../../gateway/budget.go)). A team has no right to
  spend until someone sets its budget.
- **Over budget.** This is not a failure of the system. The gateway replies
  429 `rate_limit_error` with `Retry-After` set to the seconds to the end of
  the window.
- **Database error on reserve.** The gateway replies 503 `api_error` and does
  not call the upstream. If the request context ended, the outcome is
  `canceled` and the gateway logs no error.
- **Database error on top-up.** The stream ends with an `api_error` event in
  place of the next delta
  ([`gateway/stream.go`](../../gateway/stream.go), `cutOff`). The gateway
  relays a delta only after it holds the tokens for it.
- **Database error on settle.** The gateway logs it. It cannot change the
  reply, which the client has already received. The reservation stays open.
  It stays in `reserved`, which counts against the budget, until the lease
  reaper charges it (see [0003](0003-hot-row-and-leases.md)). `Settle` stops
  the lease renewal even when it fails, so a failed settle cannot hold tokens
  for good.
- **Start.** The gateway does not start if the database is not reachable
  within 30 seconds, or if a newer release already migrated it
  ([`cmd/tokenwarden/main.go`](../../cmd/tokenwarden/main.go),
  [`internal/store/migrate.go`](../../internal/store/migrate.go)).
- **Running without budgets is a start-time choice.** `-no-budgets` turns
  enforcement off. The gateway refuses to start if both `-no-budgets` and
  `TW_DATABASE_URL` are set, or if neither is set. An operator must choose it
  in the command line. A database outage never selects it.

### Rolling upgrades across migrations

Migrations are forward only, numbered, and embedded in the binary. Each
replica runs `Migrate` at start. An advisory lock serializes concurrent
replicas, and `Migrate` returns an error when the database has a version
that the binary does not know.

The schema of [`0002_leases.sql`](../../internal/store/migrations/0002_leases.sql)
is the current example. It adds `reservations.expires_at` as `NOT NULL`, and
drops the default after it fills the old rows. During a rollout, the first
replica of the new release applies the migration while replicas of the old
release still run:

- **An old replica cannot reserve.** Its `INSERT INTO reservations` does not
  set `expires_at`, so the statement fails. The statement is one CTE, so the
  `reserved` update of the window rolls back with it, and no tokens leak.
  The gateway sees a database error and replies 503. This is the fail-closed
  path, not a silent bypass.
- **An old replica cannot restart.** `Migrate` refuses a database with a
  newer version, so a crashed old replica does not come back until it is
  replaced.
- **Old reservations are reaped.** Rows that exist at the migration get
  `expires_at = '-infinity'`, because no replica renews them. The reaper of a
  new replica charges them as held at its next pass. A request of an old
  replica that is still running then gets `ErrReservationClosed` on settle,
  and the window is charged the held amount, not the real usage.
- **The old replica can still top up** until the reaper claims its
  reservation, because the old top-up statement does not touch `expires_at`.

The rule for a future migration: a column or table that old queries do not
know must not make an old query write bad data. It may make the old query
fail. A failed query is a 503, not a wrong charge.

## Consequences

- A database outage stops all budgeted traffic with 503 and, in streams,
  with an error event. Availability of the gateway equals availability of
  the budget database for requests with budgets.
- A new team without a budget row gets 403 until an operator inserts a row.
  The Compose stack seeds demo budgets (`deploy/budgets.sql`).
- A rolling upgrade across a migration that adds a `NOT NULL` column without
  a default gives errors on old replicas until they are replaced. An
  operator must replace old replicas soon after the first new replica starts,
  or drain them first. A reaped hold of an old request is charged as held,
  which can be more or less than the real usage.
- If a future user needs the other behavior, a fail-open mode must be a new
  decision that names the risk. It must be explicit and logged, and it must
  never be a default.

## Alternatives

- **Fail open on database errors.** It keeps teams working during an outage.
  It makes the budget a hint: an attacker or a bug that breaks the database
  also removes the limit. Rejected.
- **Fail open for a team without a budget.** It makes a missing row a way
  around the limit. Rejected. A zero budget is not a valid row either:
  `tokens > 0` is a table constraint.
- **A cache of the last known state.** It would keep enforcing during a short
  outage. It needs a rule for staleness and for the hold of tokens, and it
  does not stop two replicas from each spending the cached remainder. Not
  built.
- **Expand and contract migrations.** Each migration would be safe for the
  old code: add the column as nullable, ship code that sets it, then add the
  constraint in a later release. This removes the 503 window. It costs two
  releases for each change. `0002` is a single step, so it has this 503
  window. A fleet in production should use the two-release form.
