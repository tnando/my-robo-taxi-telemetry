-- Reverses 0049. Dropping the index costs the resume probe its access path and
-- nothing else: the statement still returns the right row, by a scan.
DROP INDEX IF EXISTS idx_go_trip_legs_trip_vehicle_ended;
