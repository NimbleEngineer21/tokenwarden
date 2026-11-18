-- A reservation remembers the key ID and the model of its request, and the
-- UTC hour when it started, so that its settle or its reap can charge the
-- usage ledger. The defaults let replicas of the release before this
-- migration keep reserving during a rolling upgrade; their rows have no key
-- ID, no model, and no hour, and the ledger takes the hour from created_at.
ALTER TABLE reservations
    ADD COLUMN key_id text NOT NULL DEFAULT '' CHECK (octet_length(key_id) <= 256),
    ADD COLUMN model  text NOT NULL DEFAULT '' CHECK (octet_length(model) <= 256),
    ADD COLUMN hour   timestamptz;

-- usage_ledger counts the tokens that settled and reaped reservations
-- charged, per team, UTC hour, key ID, and model. An empty key ID or model
-- means that it is not known. The hour is the hour when the reservation
-- started, so every row falls in the same budget window as its tokens when
-- the window is a whole number of hours. The token columns of a team sum to
-- the settled counts of its windows, except for the requests that replicas
-- from before this migration settled.
--
-- estimated_tokens are tokens that the gateway charged from its own
-- estimate, because the upstream did not report them. reaped_tokens are
-- tokens that the reaper charged for a reservation whose lease ended.
CREATE TABLE usage_ledger (
    team               text        NOT NULL,
    hour               timestamptz NOT NULL,
    key_id             text        NOT NULL,
    model              text        NOT NULL,
    requests           bigint      NOT NULL DEFAULT 0 CHECK (requests >= 0),
    input_tokens       bigint      NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens      bigint      NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cache_write_tokens bigint      NOT NULL DEFAULT 0 CHECK (cache_write_tokens >= 0),
    cache_read_tokens  bigint      NOT NULL DEFAULT 0 CHECK (cache_read_tokens >= 0),
    estimated_tokens   bigint      NOT NULL DEFAULT 0 CHECK (estimated_tokens >= 0),
    reaped_tokens      bigint      NOT NULL DEFAULT 0 CHECK (reaped_tokens >= 0),
    PRIMARY KEY (team, hour, key_id, model)
);

-- The prune step and a query over all teams look rows up by hour.
CREATE INDEX usage_ledger_hour ON usage_ledger (hour);
