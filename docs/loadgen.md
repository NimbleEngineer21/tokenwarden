# loadgen

`cmd/loadgen` sends concurrent `POST /v1/messages` requests to one or more
gateways. It reports how each request ended, the token counts, and the
latency. Use it to drive a team past its budget, to load two replicas at
once, or to measure the overhead of the gateway.

## Quick start

1. Start the Compose stack (see [tokenwarden.md](tokenwarden.md#run-with-docker-compose)):

   ```sh
   docker compose -f deploy/compose.yaml up -d --build --wait
   ```

2. Send 200 streaming requests from team `red`, 50 at a time, across both
   mock replicas:

   ```sh
   TW_GATEWAY_KEY=tw_test_red_alice go run ./cmd/loadgen \
     -url http://127.0.0.1:8080,http://127.0.0.1:8082 \
     -requests 200 -concurrency 50
   ```

   The output looks like this:

   ```text
   requests  200 in 9.9s (20.1/s)
   outcomes  ok=200
   tokens    input=10400 output=13767 (1385 output/s)
   ttft      n=200 p50=377ms p95=433ms p99=458ms max=463ms
   latency   n=200 p50=2033ms p95=3198ms p99=3343ms max=3374ms
   ```

## Configuration

The gateway key comes from the environment only. A flag would put the key in
the process list and in shell history.

| Variable | Value |
|---|---|
| `TW_GATEWAY_KEY` | The gateway key. loadgen sends it in `x-api-key`. Required. |

| Flag | Default | Value |
|---|---|---|
| `-url` | `http://127.0.0.1:8080` | Gateway base URL. loadgen appends `/v1/messages`. A comma-separated list sends request 1 to the first URL, request 2 to the second, and so on. |
| `-concurrency` | `10` | Number of requests in flight. |
| `-requests` | `100` | Number of requests to send. `0` means no limit; then `-duration` is required. |
| `-duration` | `0s` | Time after which loadgen starts no new request. Requests in flight run to completion. `0` means no limit. |
| `-timeout` | `5m` | Time limit of one request. |
| `-model` | `claude-sonnet-4-5` | `model` of each request. |
| `-max-tokens` | `256` | `max_tokens` of each request. |
| `-prompt-words` | `50` | Number of words in each prompt. Each prompt starts with its request number, so that each prompt differs. |
| `-stream` | `true` | Send streaming requests. `-stream=false` sends non-stream requests. |
| `-json` | off | Print the report as one JSON object instead of text. |

loadgen stops when the request count or the duration is used up, whichever
comes first. On SIGINT or SIGTERM it cancels the requests in flight and
prints the report of what it sent.

Exit codes: 0 when the run completes, whatever the outcomes of the requests;
1 when the report cannot be written; 2 for a wrong command line.

## Report

| Line | JSON field | Value |
|---|---|---|
| `requests` | `requests`, `elapsed_seconds` | Requests sent, the run time, and the request rate. |
| `outcomes` | `outcomes` | Count of requests per outcome (see below). |
| `tokens` | `input_tokens`, `output_tokens` | Sums of the usage that the replies report. Input counts input, cache creation, and cache read tokens, the same sum that the gateway charges. A stream that ends before its `message_delta` adds its input tokens only. |
| `ttft` | `ttft_ms` | Time to the first content delta of each `ok` stream that sent content. |
| `latency` | `latency_ms` | Time from the start of each `ok` request to its last byte. |

Each latency line has the count (`n`), the nearest-rank p50, p95, p99, and
the maximum, in milliseconds.

## Outcomes

| Outcome | Meaning |
|---|---|
| `ok` | HTTP 200 and, for a stream, a `message_stop` event. |
| `http_<status>` | The gateway replied with this HTTP status. `http_429` is a budget or concurrency denial, `http_403` a team without a budget, `http_503` a limiter failure. |
| `stream_<type>` | The stream ended with an error event of this type. `stream_rate_limit_error` is a stream that the gateway cut at the budget. `stream_unknown` is an error event without a type. |
| `stream_truncated` | The stream ended before `message_stop`, without an error event. |
| `bad_body` | The reply was not valid JSON. |
| `transport` | The request did not reach the gateway, or the connection failed before the reply headers. |
| `canceled` | The request ran past `-timeout`, or the run was stopped. |
