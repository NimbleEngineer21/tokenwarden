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
