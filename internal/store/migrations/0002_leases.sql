-- expires_at is the end of a reservation's lease. The replica that holds the
-- reservation renews the lease; when the replica stops, the lease ends and
-- any replica reaps the reservation. Rows from before this migration get a
-- lease that has already ended, since no replica renews them. The default
-- is dropped so that every new row must set its own lease.
ALTER TABLE reservations ADD COLUMN expires_at timestamptz NOT NULL DEFAULT '-infinity';
ALTER TABLE reservations ALTER COLUMN expires_at DROP DEFAULT;

CREATE INDEX reservations_expires_at ON reservations (expires_at);

-- The prune step looks up reservations by window, and so does the foreign
-- key check when a window row is deleted.
CREATE INDEX reservations_window ON reservations (team, window_start, window_us);
