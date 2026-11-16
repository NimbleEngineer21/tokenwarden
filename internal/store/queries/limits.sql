-- name: UpsertBudget :exec
INSERT INTO budgets (team, tokens, window_us)
VALUES (@team, @tokens, @window_us)
ON CONFLICT (team) DO UPDATE
SET tokens = EXCLUDED.tokens, window_us = EXCLUDED.window_us;

-- name: GetBudget :one
SELECT tokens, window_us FROM budgets WHERE team = @team;

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
INSERT INTO reservations (team, window_start, window_us, budget, held)
SELECT w.team, w.window_start, w.window_us, @budget::bigint, @tokens::bigint FROM w
RETURNING id;

-- TopUp holds more tokens for an open reservation in its own window. It
-- locks the reservation first, as Settle does, so that the two cannot
-- deadlock. open is false when the reservation has settled; granted is
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
    SET held = res.held + @tokens::bigint
    WHERE res.id IN (SELECT w.id FROM w)
    RETURNING res.id
)
SELECT
    EXISTS (SELECT 1 FROM r) AS open,
    EXISTS (SELECT 1 FROM h) AS granted;

-- Settle deletes a reservation, frees what it held, and charges used
-- tokens to its window. It returns no row when the reservation has already
-- settled. settled saturates at the bigint maximum instead of raising an
-- overflow error.
-- name: Settle :one
WITH r AS (
    DELETE FROM reservations res
    WHERE res.id = @id
    RETURNING res.team, res.window_start, res.window_us, res.held
)
UPDATE usage_windows u
SET reserved = u.reserved - r.held,
    settled = LEAST(u.settled, 9223372036854775807 - @used::bigint) + @used::bigint
FROM r
WHERE u.team = r.team
  AND u.window_start = r.window_start
  AND u.window_us = r.window_us
RETURNING u.settled;
