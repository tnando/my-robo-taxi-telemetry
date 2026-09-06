-- 0047_trips.down.sql
--
-- Reverts MYR-602: drops the four Go-owned trip tables and removes the second
-- Live Activity anchor.
--
-- READ THIS BEFORE RUNNING IT.
--
-- 1. THIS REVOKES ACCESS, WHICH IS THE SAFE DIRECTION. Every participant loses
--    the trip leg of the access set the moment go_trips disappears; they fall
--    back to the plain `viewer` role their accepted share already gives them.
--    Nothing is over-exposed by this rollback. What is LOST is the window
--    itself: the owner's stated intent, who was on it, and which legs ran — and
--    none of that is recoverable, because the trip name and the leg
--    destinations are the only copies of those values anywhere.
--
-- 2. IT ENDS NOTHING ON ANY PHONE. A trip-leg Live Activity that is live on a
--    participant's lock screen is ended by a push, and this migration deletes
--    the rows the sender would have addressed it with. Those cards will sit
--    there until ActivityKit's own 8-hour staleness ceiling retires them. If
--    the intent is to undo the feature cleanly, END THE LEGS FIRST: run the
--    server with TRIPS_ENABLED=false so the sweeper closes the open windows and
--    the leg detector ends the open legs (which sends the end pushes), confirm
--    `SELECT count(*) FROM go_trip_legs WHERE ended_at IS NULL` is 0, THEN roll
--    the server back, THEN run this.
--
-- 3. FAILURE MODE IF RUN AGAINST A LIVE SERVER: every trip endpoint, the trip
--    sweeper, the leg detector and the access query's fourth UNION leg are all
--    compiled against these relations. The access query is the dangerous one —
--    it is in the WebSocket handshake path and in GET /api/vehicles — so its
--    failure takes the vehicle LIST and the SOCKET down for EVERY user, not
--    just for trip participants. Roll the SERVER back first. Always.
--
-- ── ORDER ───────────────────────────────────────────────────────────────────
--
-- go_live_activities is un-widened FIRST. The trip rows in it must be deleted
-- before `ride_request_id` can be made NOT NULL again, and the leg FK must be
-- gone before go_trip_legs can be dropped. Dropping the tables first would work
-- through the CASCADE too, but leaving the NOT NULL restore dependent on a
-- cascade's side effect is exactly the kind of implicit ordering that breaks
-- when someone reorders the file.

-- Trip-leg Activities have no anchor after this migration, so they cannot
-- survive it. They are DELETED rather than orphaned; see note 2 above about
-- what that does not do to the phone.
DELETE FROM go_live_activities WHERE trip_leg_id IS NOT NULL;

DROP INDEX IF EXISTS idx_go_live_activities_leg_live;
DROP INDEX IF EXISTS idx_go_live_activities_leg_user;

ALTER TABLE go_live_activities
    DROP CONSTRAINT IF EXISTS go_live_activities_one_anchor;

ALTER TABLE go_live_activities
    DROP COLUMN IF EXISTS trip_leg_id;

-- Safe now: every remaining row has a ride anchor, because the only rows that
-- could not were the ones deleted above.
ALTER TABLE go_live_activities
    ALTER COLUMN ride_request_id SET NOT NULL;

-- Children before parents. The FKs would cascade, but stating the order keeps
-- the file readable as a sequence rather than as a set.
DROP TABLE IF EXISTS go_trip_legs;
DROP TABLE IF EXISTS go_trip_activity_tokens;
DROP TABLE IF EXISTS go_trip_participants;
DROP TABLE IF EXISTS go_trips;
