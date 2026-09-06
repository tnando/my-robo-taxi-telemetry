-- 0048_trips_push_category.down.sql
--
-- Drops the sixth notification category.
--
-- THIS LOSES A STATED PREFERENCE AND FAILS IN THE LOUD DIRECTION. Anyone who
-- switched `trips` off has that answer stored in this column and nowhere else;
-- dropping it restores the all-on default, so re-applying 0048 later brings
-- their trip notifications BACK ON without them touching anything. There is no
-- way to avoid that with a columnar preference table — the alternative would be
-- an archive table nobody would ever read — so it is stated rather than
-- mitigated.
--
-- It is otherwise safe at any time: the server reading this column is the only
-- reader, and running the rollback against a live server makes every §7.19 read
-- and write fail (the statements name the column) rather than silently
-- misreport a preference. Roll the SERVER back first.
--
-- Migration 0047 owns go_live_activities.trip_leg_id and is untouched here; see
-- the up-file's header for why.

ALTER TABLE go_push_prefs
    DROP COLUMN IF EXISTS trips;
