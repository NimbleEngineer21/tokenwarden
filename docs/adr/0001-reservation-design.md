# 0001. Reserve, top up, and settle token budgets

Status: accepted
Date: 2026-11-17

## Context

The gateway must stop a team that used its token budget for a window. It
cannot know the cost of a request before the upstream replies:

- The input is known only as an estimate.
- The output is known only when the reply ends. A stream can run for a long
  time, and replicas share one budget.
- A client can disconnect, and a stream can end early.

A check that only reads the used count before a request lets many requests
pass at the same time. A check that only charges after a request lets a
stream run past the budget. The gateway needs a hold that is made before the
request, grows while a stream runs, and is corrected at the end.

## Decision

A request goes through three steps. The [`limit`](../../limit/limit.go)
package defines them; the gateway drives them in
[`gateway/budget.go`](../../gateway/budget.go).

1. **Reserve.** Before the gateway sends the request upstream, it holds
   tokens in the team's current window. The hold is allowed when
   `tokens <= budget - settled - reserved`. The check subtracts, so a large
   `max_tokens` from a client cannot overflow it.
   - A non-stream request holds its input estimate plus `max_tokens`.
   - A stream holds its input estimate plus `min(512, max_tokens)`.
   - A request without a positive `max_tokens` gets a 400 reply.
2. **Top up.** Before the gateway relays a delta that passes the held
   amount, it holds one more chunk of at most 512 tokens (`topUpChunk`). It
   never holds more than input estimate plus `max_tokens`. If the chunk does
   not fit, the gateway sends an error event in place of the delta and ends
   the stream. The reservation stays open until it settles.
3. **Settle.** When the reply ends, `Settle` deletes the reservation, frees
   what it held, and charges the used tokens to the window. The used count
   can be higher than the held count.

Rules that follow from these steps:

- **A reservation settles in the window where it started.** The row of that
  window is updated, even if the window has ended. A reply that crosses a
  window boundary does not charge the next window.
- **A reservation keeps its budget.** The `reservations` row stores the
  budget that was in force at the start. Top-ups check against it. A change
  by `SetBudget` applies to the next reservation
  ([`0001_limits.sql`](../../internal/store/migrations/0001_limits.sql)).
- **Each step is one SQL statement.** `Reserve`, `TopUp`, and `Settle` are
  each one conditional statement in
  [`limits.sql`](../../internal/store/queries/limits.sql). Postgres decides
  the check and the change together, so replicas cannot hold more than the
  budget.
- **Used tokens come from the upstream, merged by maximum.** The count of
  usage in a stream is cumulative. `message_start` reports input tokens, and
  `message_delta` reports the output so far. A report can omit a field.
  `Usage.Merge` keeps the larger value of each field
  ([`internal/anthropic/types.go`](../../internal/anthropic/types.go)), and
  [`gateway/stream.go`](../../gateway/stream.go) merges each report as it
  passes. A sum would count the input twice.
- **What `used` charges** ([`gateway/budget.go`](../../gateway/budget.go)):

  | Case | Tokens charged |
  |---|---|
  | The upstream status is not 2xx | 0 |
  | The reply ended with `message_stop` | Input, cache creation, cache read, and output tokens from the merged usage |
  | The reply ended with `message_stop` but no usage could be read | The held amount |
  | The reply ended early (cut-off, upstream error event, lost stream) | Input tokens (the estimate if none were reported) plus the larger of the reported output and the output estimate |

  The last row exists because the usage of a cut-off stream can lack the
  output so far.
- **The estimator is a plug-in.** `limit.Estimator` has one implementation,
  `ByteEstimator`: one token for each four bytes, rounded up. The output
  estimate counts bytes of text, partial JSON, and thinking that the client
  received.
- **The settle ignores the client.** `settle` passes
  `context.WithoutCancel`, so a client that leaves does not stop the
  charge.

## Consequences

- A window can pass its budget. A settle charges the real usage, which can
  be more than the hold. The excess is the estimation error of the requests
  in flight. The 512-token chunk limits how far a stream holds ahead of its
  estimate. It does not limit estimator error.
- A high estimate holds budget that other requests cannot use until the
  settle. A non-stream request holds its whole `max_tokens`, so a client with
  a large `max_tokens` blocks other requests of its team while the request
  runs.
- A stream costs one extra database statement for each 512 output tokens.
- Each stream, and each window, is correct in one database row. The code
  needs no distributed lock, but it needs a row that all replicas update (see
  [0003](0003-hot-row-and-leases.md)).
- If `Settle` fails after the reply, the gateway can only log the error. The
  tokens stay held until the lease reaper charges them (see 0003).
- The `limit.Limiter` interface does not change if the estimator or the
  chunk size changes. The chunk size is a constant, not an option.

## Alternatives

- **Charge after the request only.** It needs one statement, but every
  request that runs at the same time passes the check. A long stream is not
  limited at all. Rejected.
- **Hold `max_tokens` for streams too.** It is simple and never overspends by
  the output estimate. It holds far more than most replies use, so a team
  with a large `max_tokens` stops itself. Rejected for streams. It is kept
  for non-stream requests, where there is no point to top up.
- **Count tokens with the upstream's token-count endpoint.** It gives an
  exact input count but adds a round trip to every request and a dependency
  on a second upstream call. Not done. `Estimator` is the place to add it.
- **Sum the usage reports of a stream.** The reports are cumulative, so the
  sum counts tokens twice. Rejected in favor of the maximum.
- **Settle in the window where the reply ends.** It spreads one request
  over two windows and breaks the hold accounting of the first one.
  Rejected.

## Future work

A cap on requests in flight for each team (`max_inflight`) is not part of
this design. It would be a separate decision.
