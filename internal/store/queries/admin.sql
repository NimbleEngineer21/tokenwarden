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

-- ListLedger sums the usage ledger rows of hours that end after @since.
-- It groups by team, and also by hour, key ID, or model when the matching
-- flag is true; a column that is not grouped is the epoch or empty. Sums that
-- pass the bigint maximum stop at it.
-- name: ListLedger :many
SELECT
    l.team,
    (CASE WHEN sqlc.arg(by_hour)::boolean THEN l.hour ELSE 'epoch' END)::timestamptz AS hour,
    (CASE WHEN sqlc.arg(by_key)::boolean THEN l.key_id ELSE '' END)::text AS key_id,
    (CASE WHEN sqlc.arg(by_model)::boolean THEN l.model ELSE '' END)::text AS model,
    LEAST(sum(l.requests), 9223372036854775807)::bigint AS requests,
    LEAST(sum(l.input_tokens), 9223372036854775807)::bigint AS input_tokens,
    LEAST(sum(l.output_tokens), 9223372036854775807)::bigint AS output_tokens,
    LEAST(sum(l.cache_write_tokens), 9223372036854775807)::bigint AS cache_write_tokens,
    LEAST(sum(l.cache_read_tokens), 9223372036854775807)::bigint AS cache_read_tokens,
    LEAST(sum(l.estimated_tokens), 9223372036854775807)::bigint AS estimated_tokens,
    LEAST(sum(l.reaped_tokens), 9223372036854775807)::bigint AS reaped_tokens
FROM usage_ledger l
WHERE (sqlc.arg(team)::text = '' OR l.team = sqlc.arg(team)::text)
  AND l.hour + interval '1 hour' > sqlc.arg(since)::timestamptz
GROUP BY 1, 2, 3, 4
ORDER BY 1, 2 DESC, 3, 4;

-- InsertKey adds a gateway key. It changes no row when a row, revoked or
-- not, has the same key ID; the unique index gateway_keys_key_id decides,
-- so two concurrent inserts cannot both win.
-- name: InsertKey :execrows
INSERT INTO gateway_keys (sha256, team, user_name)
VALUES (sqlc.arg(sha256)::text, sqlc.arg(team)::text, sqlc.arg(user_name)::text)
ON CONFLICT ((left(sha256, 12))) DO NOTHING;

-- ListKeys returns the gateway keys by key ID, never the full hash. It
-- returns revoked keys only when all_keys is true.
-- name: ListKeys :many
SELECT left(k.sha256, 12)::text AS key_id, k.team, k.user_name, k.created_at, k.revoked_at
FROM gateway_keys k
WHERE (sqlc.arg(team)::text = '' OR k.team = sqlc.arg(team)::text)
  AND (sqlc.arg(all_keys)::boolean OR k.revoked_at IS NULL)
ORDER BY k.team, k.created_at, k.sha256;

-- RevokeKey revokes the active key with this key ID and returns its team.
-- The unique index allows at most one such key.
-- name: RevokeKey :one
UPDATE gateway_keys SET revoked_at = now()
WHERE left(sha256, 12) = sqlc.arg(key_id)::text AND revoked_at IS NULL
RETURNING team;
