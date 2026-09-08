-- 0061_trip_participant_removed_by_owner.down.sql
--
-- Drops the owner-removal marker.
--
-- IT LOSES A DECISION AND SAYS SO. After this runs, every person an owner
-- removed from a trip becomes re-addable by any participant again, and which
-- departures were removals is no longer recoverable from the roster — only
-- from the absence of a `trip.participant_added` row, which proves nothing.
-- The down migration exists because every migration in this directory has one.

ALTER TABLE go_trip_participants
    DROP COLUMN IF EXISTS removed_by_owner;
