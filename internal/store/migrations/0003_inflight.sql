-- max_inflight is the most open reservations that a team can have at once;
-- zero means no limit. inflight counts the team's open reservations. It is
-- a counter on the budget row, not a count of reservation rows: a
-- conditional UPDATE re-checks the locked row, so two concurrent Reserves
-- cannot both take the last slot.
ALTER TABLE budgets
    ADD COLUMN max_inflight bigint NOT NULL DEFAULT 0 CHECK (max_inflight >= 0),
    ADD COLUMN inflight     bigint NOT NULL DEFAULT 0 CHECK (inflight >= 0);

-- Count the reservations that are open now.
UPDATE budgets b
SET inflight = (SELECT count(*) FROM reservations r WHERE r.team = b.team);
