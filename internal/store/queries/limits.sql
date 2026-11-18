-- name: UpsertBudget :exec
INSERT INTO budgets (team, tokens, window_us, max_inflight)
VALUES (@team, @tokens, @window_us, @max_inflight)
ON CONFLICT (team) DO UPDATE
SET tokens = EXCLUDED.tokens, window_us = EXCLUDED.window_us,
    max_inflight = EXCLUDED.max_inflight;

-- name: GetBudget :one
SELECT tokens, window_us, max_inflight FROM budgets WHERE team = @team;

-- name: EnsureWindow :exec
INSERT INTO usage_windows (team, window_start, window_us)
VALUES (@team, @window_start, @window_us)
ON CONFLICT DO NOTHING;

-- Reserve holds tokens in a window and records the reservation in one
-- statement. It returns no row when the tokens do not fit. The check
-- subtracts instead of adding, so that a huge token count cannot overflow.
-- name: Reserve :one
WITH w AS (
    UPDATE usage_windows u
    SET reserved = u.reserved + @tokens::bigint
    WHERE u.team = @team
      AND u.window_start = @window_start
      AND u.window_us = @window_us
      AND sqlc.arg(tokens)::bigint <= sqlc.arg(budget)::bigint - u.settled - u.reserved
    RETURNING u.team, u.window_start, u.window_us
)
INSERT INTO reservations (team, window_start, window_us, budget, held, expires_at, key_id, model, hour)
SELECT w.team, w.window_start, w.window_us, @budget::bigint, @tokens::bigint, @expires_at::timestamptz,
       @key_id::text, @model::text, @hour::timestamptz
FROM w
RETURNING id;

-- TopUp holds more tokens for an open reservation in its own window and
-- extends its lease. It locks the reservation first, as Settle does, so
-- that the two cannot deadlock. open is false when the reservation has settled; granted is
-- false when the tokens do not fit.
-- name: TopUp :one
WITH r AS (
    SELECT res.id, res.team, res.window_start, res.window_us, res.budget
    FROM reservations res
    WHERE res.id = @id
    FOR UPDATE
), w AS (
    UPDATE usage_windows u
    SET reserved = u.reserved + @tokens::bigint
    FROM r
    WHERE u.team = r.team
      AND u.window_start = r.window_start
      AND u.window_us = r.window_us
      AND @tokens::bigint <= r.budget - u.settled - u.reserved
    RETURNING r.id
), h AS (
    UPDATE reservations res
    SET held = res.held + @tokens::bigint,
        expires_at = GREATEST(res.expires_at, @expires_at::timestamptz)
    WHERE res.id IN (SELECT w.id FROM w)
    RETURNING res.id
)
SELECT
    EXISTS (SELECT 1 FROM r) AS open,
    EXISTS (SELECT 1 FROM h) AS granted;

-- Settle deletes a reservation, frees what it held, charges used tokens to
-- its window, frees its in-flight slot, and adds the usage to the ledger
-- row of the reservation's hour, key ID, and model. used is the sum of the
-- five token kinds. It returns no row when the reservation has already
-- settled. Counts saturate at the bigint maximum instead of raising an
-- overflow error. The budget update reads the window update's rows, and the
-- ledger update reads the budget update's count, so rows lock in the order
-- reservation, window, budget, ledger, as in Reserve and the reaper. A
-- settle of zero tokens adds no ledger row.
-- name: Settle :one
WITH r AS (
    DELETE FROM reservations res
    WHERE res.id = @id
    RETURNING res.team, res.window_start, res.window_us, res.held, res.key_id, res.model,
              COALESCE(res.hour, date_trunc('hour', res.created_at, 'UTC')) AS hour
), w AS (
    UPDATE usage_windows u
    SET reserved = u.reserved - r.held,
        settled = LEAST(u.settled, 9223372036854775807 - @used::bigint) + @used::bigint
    FROM r
    WHERE u.team = r.team
      AND u.window_start = r.window_start
      AND u.window_us = r.window_us
    RETURNING u.team, u.settled
), b AS (
    UPDATE budgets bu
    SET inflight = GREATEST(bu.inflight - 1, 0)
    FROM w
    WHERE bu.team = w.team
    RETURNING bu.team
), l AS (
    INSERT INTO usage_ledger AS ul (team, hour, key_id, model, requests,
        input_tokens, output_tokens, cache_write_tokens, cache_read_tokens, estimated_tokens)
    SELECT r.team, r.hour, r.key_id, r.model, 1,
        @input::bigint, @output::bigint, @cache_write::bigint, @cache_read::bigint, @estimated::bigint
    FROM r, (SELECT count(*) FROM b) AS after_budget
    WHERE @used::bigint > 0
    ON CONFLICT (team, hour, key_id, model) DO UPDATE
    SET requests           = LEAST(ul.requests, 9223372036854775806) + 1,
        input_tokens       = LEAST(ul.input_tokens, 9223372036854775807 - EXCLUDED.input_tokens) + EXCLUDED.input_tokens,
        output_tokens      = LEAST(ul.output_tokens, 9223372036854775807 - EXCLUDED.output_tokens) + EXCLUDED.output_tokens,
        cache_write_tokens = LEAST(ul.cache_write_tokens, 9223372036854775807 - EXCLUDED.cache_write_tokens) + EXCLUDED.cache_write_tokens,
        cache_read_tokens  = LEAST(ul.cache_read_tokens, 9223372036854775807 - EXCLUDED.cache_read_tokens) + EXCLUDED.cache_read_tokens,
        estimated_tokens   = LEAST(ul.estimated_tokens, 9223372036854775807 - EXCLUDED.estimated_tokens) + EXCLUDED.estimated_tokens
)
SELECT w.settled FROM w;

-- RenewLeases extends the leases of open reservations. It never shortens a
-- lease, so a replica whose clock is behind cannot end one early.
-- name: RenewLeases :exec
UPDATE reservations res
SET expires_at = GREATEST(res.expires_at, @expires_at::timestamptz)
WHERE res.id = ANY(@ids::bigint[]);

-- ClaimExpired deletes up to max_rows reservations whose lease ended before
-- now and returns them. SKIP LOCKED lets replicas reap at the same time
-- without waiting for each other, and leaves alone a reservation that a
-- Settle or TopUp holds.
-- name: ClaimExpired :many
DELETE FROM reservations res
WHERE res.id IN (
    SELECT x.id FROM reservations x
    WHERE x.expires_at < @now::timestamptz
    ORDER BY x.id
    LIMIT sqlc.arg(max_rows)::int
    FOR UPDATE SKIP LOCKED
)
RETURNING res.team, res.window_start, res.window_us, res.held, res.key_id, res.model,
          COALESCE(res.hour, date_trunc('hour', res.created_at, 'UTC'))::timestamptz AS hour;

-- ChargeHeld moves tokens of reaped reservations from reserved to settled
-- in one window. settled saturates as in Settle.
-- name: ChargeHeld :exec
UPDATE usage_windows u
SET reserved = u.reserved - @held::bigint,
    settled = LEAST(u.settled, 9223372036854775807 - @held::bigint) + @held::bigint
WHERE u.team = @team
  AND u.window_start = @window_start
  AND u.window_us = @window_us;

-- TakeSlot takes one in-flight slot of the team. It updates no row when
-- the team is at its limit. Reserve runs it after the window update, in the
-- same transaction.
-- name: TakeSlot :execrows
UPDATE budgets b
SET inflight = b.inflight + 1
WHERE b.team = @team
  AND (b.max_inflight = 0 OR b.inflight < b.max_inflight);

-- ReleaseSlots frees in-flight slots of reaped reservations.
-- name: ReleaseSlots :exec
UPDATE budgets b
SET inflight = GREATEST(b.inflight - sqlc.arg(n)::bigint, 0)
WHERE b.team = @team;

-- PruneWindows deletes the windows that ended at or before the given time
-- and that no reservation holds.
-- name: PruneWindows :execrows
DELETE FROM usage_windows u
WHERE u.window_start + u.window_us * interval '1 microsecond' <= @ended_by::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM reservations r
      WHERE r.team = u.team
        AND r.window_start = u.window_start
        AND r.window_us = u.window_us
  );

-- ChargeLedgerReaped adds the held tokens of reaped reservations to one
-- ledger row. The reaper runs it after the window and budget updates, in
-- the same transaction. Counts saturate as in Settle.
-- name: ChargeLedgerReaped :exec
INSERT INTO usage_ledger AS ul (team, hour, key_id, model, requests, reaped_tokens)
VALUES (@team, @hour, @key_id, @model, @requests, @reaped)
ON CONFLICT (team, hour, key_id, model) DO UPDATE
SET requests      = LEAST(ul.requests, 9223372036854775807 - EXCLUDED.requests) + EXCLUDED.requests,
    reaped_tokens = LEAST(ul.reaped_tokens, 9223372036854775807 - EXCLUDED.reaped_tokens) + EXCLUDED.reaped_tokens;

-- PruneLedger deletes the ledger rows of hours that started before the
-- given time.
-- name: PruneLedger :execrows
DELETE FROM usage_ledger WHERE hour < @before::timestamptz;
