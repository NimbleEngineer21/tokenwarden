# Run the tokenwarden gateway

`cmd/tokenwarden` is the reference gateway. It proxies `POST /v1/messages`
to an Anthropic Messages API upstream. It replaces each client's gateway key
with the upstream key and writes one audit log line per request.

## Quick start with the mock upstream

1. Start the mock upstream:

   ```sh
   go run ./cmd/mockanthropic
   ```

2. Make a key file for the test key `tw_test_example` (see
   [Key file](#key-file)). Save it as `keys.json`.
3. Start the gateway:

   ```sh
   TW_UPSTREAM_URL=http://127.0.0.1:8081 \
   TW_UPSTREAM_KEY=sk-ant-test-mock \
   go run ./cmd/tokenwarden -keys keys.json
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

## Configuration

The upstream settings come from the environment only. A flag would put the
upstream key in the process list and in shell history. The gateway does not
start when either variable is empty.

| Variable | Value |
|---|---|
| `TW_UPSTREAM_URL` | Base URL of the upstream, for example `https://api.anthropic.com`. The gateway appends `/v1/messages`. |
| `TW_UPSTREAM_KEY` | The upstream API key. The gateway sends it in `x-api-key`. |

| Flag | Default | Value |
|---|---|---|
| `-addr` | `127.0.0.1:8080` | Proxy listen address. |
| `-metrics-addr` | `127.0.0.1:9090` | Metrics listen address. It must use a port other than `-addr`. |
| `-keys` | none (required) | Path of the key file. |

The gateway does not start when the key file is missing, is not valid, or
holds no keys.

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

3. Put the hash in the key file and restart the gateway.

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
| `tokenwarden_requests_total` | counter | `outcome`, `stream` | Messages API requests. `outcome` is one of `ok`, `unauthorized`, `invalid`, `upstream_error`, `gateway_error`, `incomplete`, `canceled`. |
| `tokenwarden_request_duration_seconds` | histogram | `stream` | Time from the request to the end of the reply. |
| `tokenwarden_streams_active` | gauge | none | Streams that the gateway relays now. |
| `tokenwarden_tokens_total` | counter | `team`, `kind` | Tokens that the upstream reported. `kind` is one of `input`, `output`, `cache_creation`, `cache_read`. |

## Logs

The gateway writes JSON logs to stderr. Each request to `/v1/messages` gives
one line with `"msg":"audit"`. The line holds the request IDs, `key_id`,
team, user, model, status, outcome, token usage, and duration. The gateway
never logs a key, an `Authorization` value, or prompt or reply text.
