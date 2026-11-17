-- Demo token budgets for deploy/compose.yaml. The seed service runs this
-- file after a gateway has created the schema. It does not change a budget
-- that is already set. window_us is the window length in microseconds;
-- max_inflight is the most requests in flight at once (0 = no limit).
INSERT INTO budgets (team, tokens, window_us, max_inflight)
VALUES
    ('red', 1000000, 3600000000, 0),
    ('blue', 600, 60000000, 2)
ON CONFLICT (team) DO NOTHING;
