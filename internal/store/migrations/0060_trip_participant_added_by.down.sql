-- 0060_trip_participant_added_by.down.sql
--
-- Drops the MYR-618 attribution column.
--
-- IT LOSES DATA AND SAYS SO. Every "Added by {name}" on every trip sheet is
-- gone after this runs and cannot be reconstructed from the roster — only from
-- the `trip.participant_added` audit rows, which are pruned on their own
-- retention schedule. The down migration exists because every migration in this
-- directory has one, not because rolling this one back is cheap.

ALTER TABLE go_trip_participants
    DROP COLUMN IF EXISTS added_by_user_id;
