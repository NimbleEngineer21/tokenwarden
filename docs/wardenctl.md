# wardenctl

`cmd/wardenctl` is the admin tool of tokenwarden. It sets and lists the
token budgets of teams and shows their token usage. It works on the same
Postgres database as the gateway.

## Database

The database URL comes from the environment only. A flag would put the
password in the process list and in shell history.

| Variable | Value |
|---|---|
| `TW_DATABASE_URL` | Postgres connection URL of the budget database. Required. |

wardenctl never creates or changes the schema. Before each command it checks
that the database has the schema version that this build knows:

- A database with no schema: start `tokenwarden` once; the gateway creates
  the schema.
- A database that lacks a migration: upgrade and start `tokenwarden` first.
- A database with a version that this build does not know: upgrade
  wardenctl.

In the Compose stack, Postgres has no published port. Run wardenctl as the
`wardenctl` service of the `tools` profile, which is on the database
network:

```sh
docker compose -f deploy/compose.yaml run --rm wardenctl budgets list
```

Exit codes: 0 on success, 1 when the database fails or refuses the
change, 2 for a wrong command line.

## Commands

### `wardenctl budgets set`

Creates or replaces the budget of a team. A new budget applies to the next
requests; open requests keep the budget that they started with. A new
window length starts a new count at once. Without `-max-inflight`, the
team keeps its current in-flight cap; a new team gets no cap.

```sh
wardenctl budgets set -team red -tokens 1000000 -window 1h -max-inflight 20
```

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Team name. Required. |
| `-tokens` | none | Tokens per window. Required; must be positive. |
| `-window` | none | Window length, for example `1m` or `1h`. Required; must be a positive whole number of microseconds. |
| `-max-inflight` | none | The most requests of the team in flight at once, on all replicas together. 0 means no limit. A lower cap does not stop open requests; it refuses new ones until enough of them end. When omitted, the current cap stays. |

### `wardenctl budgets list`

Lists the budget of each team. It has no flags.

```text
TEAM  TOKENS   WINDOW  MAX_INFLIGHT
blue  600      1m      2
red   1000000  1h      0
```

`MAX_INFLIGHT` 0 means no limit.

### `wardenctl budgets delete`

Deletes the budget of a team. The gateway fails closed: requests of a team
without a budget get 403 `permission_error`. Open requests of the team
settle as usual.

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Team name. Required. It is an error when the team has no budget. |

### `wardenctl usage`

Lists the token count of each window that is open or ended within
`-since`, newest first per team.

```text
TEAM  WINDOW START          WINDOW  SETTLED  RESERVED  BUDGET  OPEN
blue  2026-11-17T15:04:00Z  1m      410      150       600     2
red   2026-11-17T15:00:00Z  1h      38210    1024      1000000  2
```

| Column | Value |
|---|---|
| `SETTLED` | Tokens of requests that ended in this window. |
| `RESERVED` | Tokens that open requests hold in this window. |
| `BUDGET` | The current budget of the team, or `-` when the team has no budget with this window length. |
| `OPEN` | Open reservations in this window. |

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Show only this team. |
| `-since` | `24h` | Show windows that ended less than this long ago. |
