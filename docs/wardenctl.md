# wardenctl

`cmd/wardenctl` is the admin tool of tokenwarden. It sets and lists the
token budgets of teams, shows their token usage, and creates, lists, and
revokes gateway keys. It works on the same Postgres database as the
gateway. The key commands apply to a gateway that runs with `-db-keys`.

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

With `-by` or `-hourly`, it lists the usage ledger instead: the tokens
that ended requests used, summed per team and per the chosen key ID,
model, or hour. The gateway writes the ledger when a request settles or
when the lease reaper charges it, in the same transaction as the window
count. A row counts in the UTC hour when its request started.

```text
$ wardenctl usage -by key,model
TEAM  KEY           MODEL            REQUESTS  INPUT  OUTPUT  CACHE_WRITE  CACHE_READ  ESTIMATED  REAPED  TOTAL
red   3f9a01c2b7de  claude-test-a    12        4810   9230    0            1200        0          0       15240
red   3f9a01c2b7de  claude-test-b    3         900    1500    0            0           64         512     2976
```

| Column | Value |
|---|---|
| `KEY`, `MODEL` | `-` when the gateway did not know it, for example for a request of a replica from before the ledger. |
| `INPUT`, `OUTPUT`, `CACHE_WRITE`, `CACHE_READ` | Tokens that the upstream reported. |
| `ESTIMATED` | Tokens that the gateway charged from its own estimate, because the upstream did not report them, for example for a cut-off stream. |
| `REAPED` | Tokens that the lease reaper charged for requests whose replica stopped. |
| `TOTAL` | The sum of the token columns. |

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Show only this team. |
| `-since` | `24h` | Show windows or hours that ended less than this long ago. |
| `-by` | none | Show the usage ledger per `key`, `model`, or `key,model`. |
| `-hourly` | `false` | Show the usage ledger per hour. |

### `wardenctl keys create`

Creates a gateway key for a team. The key is `tw_` and 43 base64url
characters (256 random bits). The database holds only its SHA-256 hash.

```sh
key=$(wardenctl keys create -team red -user alice)
```

stdout gets the key and nothing else, so that a script can capture it.
stderr gets the key ID and a note that the key is not shown again. Store
the key at once: wardenctl cannot show it again.

The key works on the next request, on every replica. A key of a team
without a budget is valid, but its requests get 403 `permission_error`
until the team has a budget; wardenctl prints a warning to stderr.

The key ID is the first 12 hex characters of the hash. The database
refuses two keys with the same key ID, revoked keys included, so a key ID
names one key. On such a clash, wardenctl makes a new key, up to 3 times.

If wardenctl cannot write the key to stdout, it revokes the key, because
nobody has it, and exits 1.

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Team that owns the key. Required. |
| `-user` | none | Person or service that holds the key. It shows in the audit log. |

### `wardenctl keys list`

Lists the gateway keys by key ID. It never shows a key or its full hash.

```text
KEY ID        TEAM  USER   CREATED               REVOKED
034a44af6ff9  red   alice  2026-11-17T15:00:00Z  -
```

`USER` is `-` when the key has no user. `REVOKED` is the revoke time, or
`-` for an active key.

| Flag | Default | Value |
|---|---|---|
| `-team` | none | Show only this team. |
| `-all` | `false` | Also show revoked keys. |

### `wardenctl keys revoke`

Revokes a key. The gateway does not cache keys: the next request with the
key gets 401, on every replica. Open requests of the key finish. The row
stays, so that audit lines with the key ID still name a team and a user.

```sh
wardenctl keys revoke -id 034a44af6ff9
```

It is an error when no active key has the key ID.

| Flag | Default | Value |
|---|---|---|
| `-id` | none | Key ID from `keys list`: 12 lowercase hex characters. Required. |
