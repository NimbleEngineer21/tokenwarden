-- budgets holds the token budget of each team. window_us is the window
-- length in microseconds, the precision of a Postgres timestamp. Its upper
-- bound keeps the length inside a Go time.Duration.
CREATE TABLE budgets (
    team      text PRIMARY KEY CHECK (team <> ''),
    tokens    bigint NOT NULL CHECK (tokens > 0),
    window_us bigint NOT NULL CHECK (window_us > 0 AND window_us <= 9223372036854775)
);

-- usage_windows counts the tokens of one team in one window. The window
-- length is part of the key, so that a changed budget window never mixes
-- two windows that start at the same instant.
CREATE TABLE usage_windows (
    team         text        NOT NULL,
    window_start timestamptz NOT NULL,
    window_us    bigint      NOT NULL,
    settled      bigint      NOT NULL DEFAULT 0 CHECK (settled >= 0),
    reserved     bigint      NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    PRIMARY KEY (team, window_start, window_us)
);

-- reservations holds each open reservation. A row is deleted when its
-- reservation settles. budget is the team budget when the reservation
-- started; top-ups check against it.
CREATE TABLE reservations (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    team         text        NOT NULL,
    window_start timestamptz NOT NULL,
    window_us    bigint      NOT NULL,
    budget       bigint      NOT NULL CHECK (budget > 0),
    held         bigint      NOT NULL CHECK (held > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (team, window_start, window_us)
        REFERENCES usage_windows (team, window_start, window_us)
);
