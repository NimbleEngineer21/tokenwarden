-- Demo gateway keys for deploy/compose.yaml. The seed service runs this
-- file after a gateway has created the schema. It holds the SHA-256 hash of
-- each key, never the key: tw_test_red_alice (team red) and
-- tw_test_blue_bob (team blue). It does not change a key that is already
-- there.
INSERT INTO gateway_keys (sha256, team, user_name)
VALUES
    ('034a44af6ff94274c8c41c22745c852ee6c9fe247aa01cad693b33da65fd4c06', 'red', 'alice'),
    ('0529232823f36e8e0259137ec306f7aabe1f67c32335a1e8db511b1b046fac7a', 'blue', 'bob')
ON CONFLICT (sha256) DO NOTHING;
