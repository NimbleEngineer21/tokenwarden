-- A key ID, the first 12 hex characters of the key hash, names one key,
-- revoked keys included, so that an audit line and `wardenctl keys revoke`
-- can never point at two keys. The unique index also makes the lookup by
-- key ID an index scan.
--
-- Two rows can share a key ID only if both were added by hand. Stop with a
-- message that names the key ID, not with the bare unique-index error.
DO $$
DECLARE
    clash text;
BEGIN
    SELECT left(sha256, 12) INTO clash
    FROM gateway_keys
    GROUP BY left(sha256, 12)
    HAVING count(*) > 1
    LIMIT 1;
    IF clash IS NOT NULL THEN
        RAISE EXCEPTION 'gateway_keys: more than one key has the key ID %; delete all but one of them, then start again', clash;
    END IF;
END
$$;

CREATE UNIQUE INDEX gateway_keys_key_id ON gateway_keys (left(sha256, 12));
