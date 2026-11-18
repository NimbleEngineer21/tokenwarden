# Run the tokenwarden gateway

`cmd/tokenwarden` is the reference gateway. It proxies `POST /v1/messages`
to an Anthropic Messages API upstream. It replaces each client's gateway key
with the upstream key, enforces each team's token budget, and writes one
audit log line per request.

## Quick start with the mock upstream

1. Start the mock upstream:

   ```sh
   go run ./cmd/mockanthropic
   ```

2. Make a key file for the test key `tw_test_example` (see
   [Key file](#key-file)). Save it as `keys.json`.
3. Start the gateway. `-no-budgets` turns the token budgets off, so that
   no database is necessary. To use budgets, see [Token budgets](#token-budgets)
   or [Run with Docker Compose](#run-with-docker-compose).

   ```sh
   TW_UPSTREAM_URL=http://127.0.0.1:8081 \
   TW_UPSTREAM_KEY=sk-ant-test-mock \
   go run ./cmd/tokenwarden -keys keys.json -no-budgets
   ```

4. Send a request:

   ```sh
   curl -sN http://127.0.0.1:8080/v1/messages \
     -H 'x-api-key: tw_test_example' \
     -H 'anthropic-version: 2023-06-01' \
     -H 'content-type: application/json' \
     -d '{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,
          "messages":[{"role":"user","content":"Hello"}]}'
   ```

## Run with Docker Compose

`deploy/compose.yaml` starts two gateway replicas, the mock upstream, and a
Postgres server that holds the budgets of both replicas. It needs Docker
with Compose v2. Run the commands from the repo root.

```sh
docker compose -f deploy/compose.yaml up --build
```

| Service | Host address | Upstream |
|---|---|---|
| `gateway-a` | `127.0.0.1:8080` | mock |
| `gateway-b` | `127.0.0.1:8082` | mock |
| `gateway-a-real` | `127.0.0.1:8180` | `https://api.anthropic.com` (profile `real`) |
| `gateway-b-real` | `127.0.0.1:8182` | `https://api.anthropic.com` (profile `real`) |

The gateways read their keys from Postgres (`-db-keys`). There are two test
keys: `tw_test_red_alice` (team `red`) and `tw_test_blue_bob` (team `blue`).

The `seed` service waits until a gateway creates the schema, and then runs
`deploy/keys.sql` and `deploy/budgets.sql` in one transaction. It does not
change a key or a budget that is already set.

| Team | Budget |
|---|---|
| `red` | 1,000,000 tokens per hour |
| `blue` | 600 tokens per minute. A request with `max_tokens` above about 550 gets 429, and a stream that is longer than about 500 tokens is cut. |

> **Warning:** The Postgres password in `deploy/compose.yaml` is fixed and
> public. Use the stack for a demo only. Postgres has no published port.

The stack publishes only the proxy ports, and only on 127.0.0.1. The mock and
the metrics listeners stay on Docker networks:

- The mock and Postgres are on networks with no route out of the host. The
  mock `/_faults` API has no auth.
- The metrics carry team names.

The `probe` service reads them from inside the networks:

```sh
docker compose -f deploy/compose.yaml run --rm probe -s http://gateway-a:9090/metrics
```

To use the real API:

1. Copy `deploy/.env.example` to `deploy/.env`. Git ignores `deploy/.env`.
2. Set `ANTHROPIC_API_KEY` in `deploy/.env`.
3. Start the stack with the `real` profile:

   ```sh
   docker compose -f deploy/compose.yaml --profile real up --build
   ```

The real gateways cannot reach the mock, and the mock gateways never use the
real API. When `ANTHROPIC_API_KEY` is empty, the real gateways do not start.

`make smoke` starts the stack, sends a stream and a non-stream request
through each mock replica, checks the audit lines, the metrics, and the
published ports, runs team `blue` past its budget, and then stops the
stack. The `smoke` workflow runs it on
each pull request.

## Configuration

The upstream and database settings come from the environment only. A flag
would put the upstream key or the database password in the process list and
in shell history. The gateway does not start when `TW_UPSTREAM_URL` or
`TW_UPSTREAM_KEY` is empty.

| Variable | Value |
|---|---|
| `TW_UPSTREAM_URL` | Base URL of the upstream, for example `https://api.anthropic.com`. The gateway appends `/v1/messages`. |
| `TW_UPSTREAM_KEY` | The upstream API key. The gateway sends it in `x-api-key`. |
| `TW_DATABASE_URL` | Postgres connection URL of the database of budgets and, with `-db-keys`, of keys, for example `postgres://tokenwarden@db.internal:5432/tokenwarden`. Required with `-db-keys`, and without `-no-budgets`. |

| Flag | Default | Value |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Proxy listen address. |
| `-metrics-addr` | `127.0.0.1:9090` | Metrics listen address. It must use a port other than `-addr`. |
| `-keys` | none | Path of the [key file](#key-file). Give this flag or `-db-keys`, not both. |
| `-db-keys` | off | Read the keys from the database of `TW_DATABASE_URL` ([Keys in Postgres](#keys-in-postgres)). |
| `-no-budgets` | off | Enforce no token budgets. Without `-db-keys`, the gateway does not start when this flag and `TW_DATABASE_URL` are both set, or when neither is set. With `-db-keys`, the database holds only the keys. |

The gateway does not start when:

- The key file is missing, is not valid, or holds no keys.
- The database is not available within 30 seconds, or a newer release of
  the gateway migrated it.

On SIGINT or SIGTERM, the gateway stops accepting connections and lets open
requests run for 10 seconds. Then it cuts the streams that are still open,
logs a warning, and exits 0. A second signal stops the gateway at once, with
the default status for that signal (130 for SIGINT, 143 for SIGTERM).

Exit codes: 0 after a stop signal, 1 when the gateway cannot start or a
listener fails, 2 for a wrong command line.

## Key file

The key file holds the SHA-256 hash of each gateway key, never the key. Each
entry has a team and an optional user. Budgets and policy apply per team.

```json
{
  "keys": [
    {
      "sha256": "5f08e825ca7b5a5a125e1cdae3abb568648ede29d8c679cd0f46f5f04aab7387",
      "team": "red",
      "user": "alice"
    }
  ]
}
```

The hash above is the hash of the test key `tw_test_example`. Rules:

- `sha256` is 64 lowercase hex characters.
- `team` is required.
- An unknown field is an error, so a raw key under a wrong field name is
  refused.
- A duplicate hash is an error.

The gateway logs a key as `key_id`: the first 12 hex characters of its hash.

### Make and hash a key

1. Make a random key. Give it to the client over a secure channel:

   ```sh
   printf 'tw_%s\n' "$(openssl rand -hex 24)"
   ```

2. Hash the key. `read -rs` keeps the key off the screen and out of shell
   history. `printf` is a shell builtin, so the key does not show in the
   process list. Do not use `echo`: it adds a newline, and the hash is then
   wrong.

   ```sh
   read -rs KEY && printf '%s' "$KEY" | shasum -a 256 | cut -d' ' -f1; unset KEY
   ```

   On Linux, `sha256sum` gives the same output as `shasum -a 256`.

3. Put the hash in the key file and restart the gateway, or add it to the
   database ([Keys in Postgres](#keys-in-postgres)).

## Keys in Postgres

With `-db-keys`, the gateway reads the keys from the `gateway_keys` table.
All replicas share the table, and a change needs no restart. The table
holds the hash of each key, never the key:

| Column | Value |
|---|---|
| `sha256` | The hash of the key, as in the key file. Primary key. |
| `team` | The team of the key. Required. |
| `user_name` | The user of the key. Empty when not set. |
| `created_at` | When the row was added. |
| `revoked_at` | When the key was revoked, or null. |

The key ID, the first 12 hex characters of `sha256`, is unique, revoked
keys included: an insert of a second key with the same key ID fails. An
audit line with a `key_id` names one key.

Each request reads its key with one primary-key query, with no cache. So a
new key works, and a revoked key fails, from the next request on. A
database failure gives 500 `api_error`, never access.

Make, list, and revoke keys with [wardenctl](wardenctl.md#wardenctl-keys-create):

```sh
key=$(wardenctl keys create -team red -user alice)
wardenctl keys list -team red
wardenctl keys revoke -id 034a44af6ff9
```

Without wardenctl, use SQL. Add a key with its hash
([Make and hash a key](#make-and-hash-a-key)):

```sql
INSERT INTO gateway_keys (sha256, team, user_name)
VALUES ('5f08e825ca7b5a5a125e1cdae3abb568648ede29d8c679cd0f46f5f04aab7387', 'red', 'alice');
```

Revoke a key. The row stays, so an old audit line with its `key_id` still
names its team and user:

```sql
UPDATE gateway_keys SET revoked_at = now()
WHERE sha256 = '5f08e825ca7b5a5a125e1cdae3abb568648ede29d8c679cd0f46f5f04aab7387';
```

At start, the gateway logs the number of active keys. Zero is a warning,
not an error: in a new deployment, the keys are added after the first
gateway creates the schema.

## Token budgets

Each team has a budget: a number of tokens in each window of a fixed
length. Windows start at whole multiples of their length from the Unix
epoch, in UTC. The budget counts input, cache, and output tokens. All
replicas that use one database share the budgets.

At start, the gateway creates or updates the schema in `TW_DATABASE_URL`.
Replicas that start at the same time wait for each other. The gateway does
not set budgets. Set them with [wardenctl](wardenctl.md):

```sh
wardenctl budgets set -team red -tokens 1000000 -window 1h -max-inflight 20
```

Or set them with SQL. `window_us` is the window length in microseconds.
`max_inflight` is the most requests of the team that can be in flight at
once, on all replicas together; 0 means no limit:

```sql
INSERT INTO budgets (team, tokens, window_us, max_inflight)
VALUES ('red', 1000000, 3600000000, 20)
ON CONFLICT (team) DO UPDATE
SET tokens = EXCLUDED.tokens, window_us = EXCLUDED.window_us,
    max_inflight = EXCLUDED.max_inflight;
```

A new budget applies to the next requests. Open requests keep the budget
that they started with. A lower `max_inflight` does not stop open
requests; it refuses new ones until enough of them end.

The gateway holds tokens before it sends a request upstream, and corrects
the hold when the reply ends:

- A non-stream request holds its input guess plus `max_tokens`.
- A stream holds its input guess plus up to 512 output tokens, and holds
  512 more each time the reply needs them.

With budgets on, each request must have a positive `max_tokens`. The
replies:

| Case | Reply |
|---|---|
| The first hold does not fit in the budget. | 429 `rate_limit_error` with `Retry-After`: the seconds to the end of the window. |
| The team has `max_inflight` requests in flight. | 429 `rate_limit_error`, with no `Retry-After`. |
| The team has no budget. | 403 `permission_error`. |
| A stream needs a hold that does not fit. | The stream ends with a `rate_limit_error` event in place of the next delta. |
| The database fails. | 503 `api_error`, or an `api_error` event in a stream. The gateway fails closed. |

### Leases

Each hold has a 30-second lease. The replica that holds it renews the lease
every 10 seconds until the reply ends. If a replica stops before a reply
ends, for example in a crash, its leases end. Within about 40 seconds, any
replica then reaps the holds: it counts their held tokens as used, since
the requests may have used them upstream. Each reap gives a `WARN` log line
and adds to the `tokenwarden_reaped_*` metrics. A stream that the shutdown
grace period cuts is reaped the same way if its replica stopped first.

Replica clocks must agree to well within the shortest budget window and the
lease length.

The gateway deletes the usage counts of a window 7 days after the window
ends.

## Endpoints

| Listener | Path | Purpose |
|---|---|---|
| `-addr` | `POST /v1/messages` | The proxied Messages API. |
| `-addr` | `GET /healthz` | Returns 200 `ok` when the process is up. It does not call the upstream. |
| `-metrics-addr` | `GET /metrics` | Prometheus metrics. |

## Metrics

The metrics listener has its own registry. It also exports the standard Go
runtime (`go_*`) and process (`process_*`) metrics.

The metrics carry team names. Keep `-metrics-addr` on a loopback or private
interface.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `tokenwarden_requests_total` | counter | `outcome`, `stream` | Messages API requests. `outcome` is one of `ok`, `unauthorized`, `invalid`, `upstream_error`, `gateway_error`, `incomplete`, `canceled`, `budget_exceeded`. |
| `tokenwarden_request_duration_seconds` | histogram | `stream` | Time from the request to the end of the reply. |
| `tokenwarden_streams_active` | gauge | none | Streams that the gateway relays now. |
| `tokenwarden_tokens_total` | counter | `team`, `kind` | Tokens that the upstream reported. `kind` is one of `input`, `output`, `cache_creation`, `cache_read`. |
| `tokenwarden_budget_denials_total` | counter | `team`, `stage` | Requests that the token budget stopped. `stage` is `reserve` (429 or 403 before the upstream call), `concurrency` (429 at the in-flight limit), or `topup` (a stream cut). |
| `tokenwarden_topups_total` | counter | `result` | Stream holds after the first one. `result` is one of `ok`, `exceeded`, `error`. |
| `tokenwarden_reaped_reservations_total` | counter | none | Holds that the gateway reaped after their lease ended. Only with budgets on. |
| `tokenwarden_reaped_tokens_total` | counter | none | Tokens that the reaped holds held. They count as used. Only with budgets on. |

## Logs

The gateway writes JSON logs to stderr. Each request to `/v1/messages` gives
one line with `"msg":"audit"`. The line holds the request IDs, `key_id`,
team, user, model, status, outcome, token usage, and duration. `time` is when
the gateway wrote the line. `event_time` is when the request started. The
gateway never logs a key, an `Authorization` value, or prompt or reply text.
