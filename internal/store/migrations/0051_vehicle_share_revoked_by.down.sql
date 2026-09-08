-- Reverses 0051. The authorship of every tombstone written while it was in
-- place is lost, which is why the up-migration treats NULL as "unknown".
DROP INDEX IF EXISTS idx_go_vehicle_shares_revoked_grantee;

ALTER TABLE go_vehicle_shares
    DROP CONSTRAINT IF EXISTS go_vehicle_shares_revoked_by_check;

ALTER TABLE go_vehicle_shares
    DROP COLUMN IF EXISTS revoked_reason,
    DROP COLUMN IF EXISTS revoked_by;
