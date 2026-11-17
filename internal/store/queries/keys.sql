-- name: GetActiveKey :one
SELECT team, user_name FROM gateway_keys
WHERE sha256 = @sha256 AND revoked_at IS NULL;

-- name: CountActiveKeys :one
SELECT count(*) FROM gateway_keys WHERE revoked_at IS NULL;
