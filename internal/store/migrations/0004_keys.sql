-- gateway_keys holds the gateway keys that the gateway reads with
-- -db-keys. It holds the SHA-256 hash of each key, never the key. A
-- revoked key keeps its row, so that an audit line with its key ID still
-- names a team and a user. A key of a team without a budget is allowed:
-- its requests get 403, as with the key file.
CREATE TABLE gateway_keys (
    sha256     text PRIMARY KEY CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    team       text NOT NULL CHECK (team <> ''),
    user_name  text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz
);
