-- Demo token budgets for deploy/compose.yaml. The seed service runs this
-- file after a gateway has created the schema. It does not change a budget
-- that is already set. window_us is the window length in microseconds.
INSERT INTO budgets (team, tokens, window_us)
VALUES
    ('red', 1000000, 3600000000),
    ('blue', 600, 60000000)
ON CONFLICT (team) DO NOTHING;
