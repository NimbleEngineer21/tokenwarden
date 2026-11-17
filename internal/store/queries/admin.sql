-- Queries of the wardenctl admin tool. Each lists its columns, so that a
-- column that a later migration adds does not change the result rows.

-- name: ListBudgets :many
SELECT b.team, b.tokens, b.window_us, b.max_inflight FROM budgets b ORDER BY b.team;

-- name: DeleteBudget :execrows
DELETE FROM budgets WHERE team = @team;

-- ListUsage returns the usage windows that end after @since, newest first
-- per team. budget_tokens is the current budget of the team when its
-- window length matches the row, else NULL.
-- name: ListUsage :many
SELECT
    u.team,
    u.window_start,
    u.window_us,
    u.settled,
    u.reserved,
    b.tokens AS budget_tokens,
    (SELECT count(*) FROM reservations r
     WHERE r.team = u.team AND r.window_start = u.window_start AND r.window_us = u.window_us
    ) AS open_reservations
FROM usage_windows u
LEFT JOIN budgets b ON b.team = u.team AND b.window_us = u.window_us
WHERE (sqlc.arg(team)::text = '' OR u.team = sqlc.arg(team)::text)
  AND u.window_start + u.window_us * interval '1 microsecond' > sqlc.arg(since)::timestamptz
ORDER BY u.team, u.window_start DESC;
