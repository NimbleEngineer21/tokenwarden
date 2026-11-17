# 0003. One counter row for each team window, with leased reservations

Status: accepted
Date: 2026-11-17

## Context

Replicas share each team's budget through Postgres. Two things follow:

1. **The counters need one place to check and change atomically.** The check
   `settled + reserved + tokens <= budget` and the change to `reserved` must
   happen together. Otherwise two replicas can both see room and both hold.
2. **A replica can stop in the middle of a request.** A crash, a kill, or a
   shutdown that cuts a stream leaves a reservation that nothing settles.
   Its tokens would stay in `reserved` for ever, and the team would lose that
   part of its budget until the window ends.

## Decision

### One row for each team and window

Table `usage_windows` has one row for each `(team, window_start, window_us)`,
with the columns `settled` and `reserved`
([`0001_limits.sql`](../../internal/store/migrations/0001_limits.sql)). The
window length is part of the key, so a changed window length starts a new
count and never mixes with an old one that starts at the same instant.

`Reserve`, `TopUp`, `Settle`, and the reaper all change this row
([`limits.sql`](../../internal/store/queries/limits.sql)):

- `Reserve` is `UPDATE usage_windows ... WHERE <tokens fit>`, and the
  `reservations` insert is in the same statement. If the tokens do not fit,
  the `UPDATE` matches no row, and the `INSERT` does not run.
- `Reserve` first runs `INSERT ... ON CONFLICT DO NOTHING` to make the row of
  the window. Rows are made on first use, not in advance.
- `TopUp` and `Settle` lock the `reservations` row first, then update the
  window row. Both use this order, so they cannot deadlock each other.
- The reaper claims reservations first, then updates windows in sorted key
  order ([`limit/postgres/lease.go`](../../limit/postgres/lease.go),
  `reapBatch`), so two reapers cannot deadlock on windows.
- `Reserve`, `TopUp`, and `Settle` each run as one statement in its own
  implicit transaction. A row lock lasts for one statement, not for the
  length of a request. The reaper is the exception: it holds its window row
  locks until the end of one batch transaction (at most 100 reservations).
- The row is deleted by `PruneWindows` when the window ended more than the
  retention ago (default 7 days) and no reservation refers to it.

The window comes from the Go clock of the replica that reserves, not from
the database clock. Replica clocks must agree to well within the window
length.

### Leases and the reaper

Each `reservations` row has `expires_at`
([`0002_leases.sql`](../../internal/store/migrations/0002_leases.sql)).

- `Reserve` sets it to now plus the lease TTL (default 30 seconds,
  `WithLeaseTTL`). `TopUp` extends it.
- `Limiter.Run` runs three loops. Every TTL/3 (10 seconds by default):
  - **Renew.** One statement extends every open reservation of this limiter,
    with `id = ANY(ids)`. The limiter tracks its open reservations in
    memory. The statement uses `GREATEST`, so a replica with a slow clock
    cannot shorten a lease.
  - **Reap.** It deletes up to 100 reservations whose lease ended, in one
    transaction, with `FOR UPDATE SKIP LOCKED`. It then moves the held tokens
    of each window from `reserved` to `settled`. Every replica runs the
    reaper. `SKIP LOCKED` lets them run at the same time. It also skips a
    reservation that a `Settle` or `TopUp` is changing.
  - Every minute, **prune** old windows.
- A stopped replica's reservations are reaped between one TTL and 4/3 of a
  TTL after its last renewal, so about 40 seconds by default.
- The reaper **charges** the held tokens. The request may have used them
  upstream, and the reaper cannot know. The gateway logs each reap as a
  warning and counts `tokenwarden_reaped_reservations_total` and
  `tokenwarden_reaped_tokens_total`.
- After a reap, `Settle` and `TopUp` on that reservation return
  `ErrReservationClosed`. The top-up cuts the stream.
- The loop outlives the signal context in the binary, so it keeps renewing
  during the shutdown grace period
  ([`cmd/tokenwarden/main.go`](../../cmd/tokenwarden/main.go),
  `startLeases`).
- Every `Limiter` that reserves must run `Run`. If one limiter does not renew
  its leases, the reaper of another limiter reaps its open reservations.
- The in-memory limiter has no leases. Its state ends with its process.

## Consequences

- **The row is a hot spot.** Every request of a team takes the same row lock
  for each reserve, each top-up (one for each 512 output tokens of a stream),
  and each settle. Statements on one team's row run one after the other.
  Throughput for one team is limited by the rate of single-row updates in
  Postgres. Teams do not block each other. The repository has no measured
  throughput figure, and this ADR does not give one. Measure with a load
  test before you decide that the row is a problem.
- **A short lock time is the main mitigation.** The row lock lasts for one
  statement, and no request holds a lock while it waits for the upstream.
  The 512-token chunk keeps the number of top-ups for a long stream low.
- **A crash costs budget, not correctness.** A crashed replica's holds are
  charged in full after about 40 seconds. Until then they count as reserved.
  A team can lose up to the held amount of each lost request until the
  window ends.
- **A long pause can lose a live request's hold.** A replica that cannot
  reach the database for more than the TTL (a stop-the-world pause, or a
  network cut) has its leases reaped although its requests still run. These
  requests then fail to top up or settle. Their charge is the held amount.
- **The lease depends on clocks.** Expiry compares the Go clock of the
  renewing replica with the Go clock of the reaping replica. Large skew can
  reap a live request (reaper ahead) or delay a reap (reaper behind). This
  is in `docs/tokenwarden.md`.
- **Reaping adds write load.** One renew statement for each replica every 10
  seconds, and a reap query every 10 seconds on every replica. The reap uses
  the index on `expires_at`. The prune check uses the index on
  `reservations (team, window_start, window_us)`, as does the foreign key
  check when a window row is deleted.
- **Reaped tokens are counted as used.** They are not refunded if the upstream
  never saw the request. This errs on the side of the budget.

## Alternatives

- **Sharded counters.** Split the team's row into N rows and reserve from a
  random shard. This reduces contention on one row. It makes the budget
  check inexact: a shard can say no while the others have room, so the
  gateway needs a rule to spill to another shard, and a team with a small
  budget cannot be split. Not built. It is the next step if a measured team
  rate shows the single row is the limit.
- **Hold nothing in the database; keep counts in each replica.** Each replica
  gets a share of the budget. It needs no database on the request path. It
  strands tokens on idle replicas and allows a team to spend N times its
  budget if the sharing is wrong. Rejected for a shared budget.
- **A Redis counter.** It is fast, but it adds a second store with different
  durability, and the reservation, the settle, and the reap must still be
  atomic. Postgres is already required for the budgets. Not chosen.
- **Row lock for the length of the request** (`SELECT ... FOR UPDATE` held
  while the stream runs). It removes the need for leases, because Postgres
  releases the lock when the connection dies. It holds a connection for each
  open stream and serializes a team's requests. Rejected.
- **No lease; reap by age.** A reaper that deletes reservations older than a
  fixed time would cut or charge long streams that still run. A lease that
  the owner renews ends only when the owner stops.
- **Release the held tokens on reap, not charge them.** It would be kinder
  after a crash. It lets a team that triggers crashes spend without limit.
  Rejected.

## Future work

A cap on requests in flight for each team (`max_inflight`) would add a
count to the same row and its own reservation rule. It is not part of this
decision.
