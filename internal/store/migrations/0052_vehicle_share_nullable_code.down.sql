-- Reverses 0052. Restoring NOT NULL requires the rows that 0052 allowed to
-- exist to be given values again, so the down-migration fills the NULLs with
-- inert ones first: an empty code (unredeemable — `ValidShareCodeFormat`
-- rejects it and no redeem predicate matches it) and the row's own
-- created_at as an expiry that is already in the past by definition.
ALTER TABLE go_vehicle_shares
    DROP CONSTRAINT IF EXISTS go_vehicle_shares_pending_credential_check;

UPDATE go_vehicle_shares SET code = '' WHERE code IS NULL;
UPDATE go_vehicle_shares SET expires_at = created_at WHERE expires_at IS NULL;

ALTER TABLE go_vehicle_shares
    ALTER COLUMN code       SET NOT NULL,
    ALTER COLUMN expires_at SET NOT NULL;
