-- Reverses 0050. Dropping the column re-opens the duplicate-card window the
-- catch-up path needs it for, so the catch-up must be disabled with it.
ALTER TABLE go_trip_activity_tokens
    DROP COLUMN IF EXISTS started_leg_id;
